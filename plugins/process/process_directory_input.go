/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012-2014
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Victor Ng (vng@mozilla.com)
#   Rob Miller (rmiller@mozilla.com)
#
#***** END LICENSE BLOCK *****/

package process

import (
	"errors"
	"fmt"
	"github.com/bbangert/toml"
	. "github.com/mozilla-services/heka/pipeline"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessEntry struct {
	ir     InputRunner
	maker  MutableMaker
	config *ProcessInputConfig
}

type ProcessDirectoryInputConfig struct {
	// Root folder of the tree where the scheduled jobs are defined.
	ProcessDir string `toml:"process_dir"`

	// Number of seconds to wait between scans of the job directory. Defaults
	// to 300.
	TickerInterval uint `toml:"ticker_interval"`
}

type ProcessDirectoryInput struct {
	// The actual running InputRunners.
	inputs map[string]*ProcessEntry
	// Set of InputRunners that should exist as specified by walking
	// the process directory.
	specified map[string]*ProcessEntry
	stopChan  chan bool
	procDir   string
	ir        InputRunner
	h         PluginHelper
	pConfig   *PipelineConfig
}

// Heka will call this before calling any other methods to give us access to
// the pipeline configuration.
func (pdi *ProcessDirectoryInput) SetPipelineConfig(pConfig *PipelineConfig) {
	pdi.pConfig = pConfig
}

func (pdi *ProcessDirectoryInput) Init(config interface{}) (err error) {
	conf := config.(*ProcessDirectoryInputConfig)
	pdi.inputs = make(map[string]*ProcessEntry)
	pdi.stopChan = make(chan bool)
	globals := pdi.pConfig.Globals
	pdi.procDir = filepath.Clean(globals.PrependShareDir(conf.ProcessDir))
	return
}

// ConfigStruct implements the HasConfigStruct interface and sets defaults.
func (pdi *ProcessDirectoryInput) ConfigStruct() interface{} {
	return &ProcessDirectoryInputConfig{
		ProcessDir:     "processes",
		TickerInterval: 300,
	}
}

func (pdi *ProcessDirectoryInput) Stop() {
	close(pdi.stopChan)
}

// CleanupForRestart implements the Restarting interface.
func (pdi *ProcessDirectoryInput) CleanupForRestart() {
	pdi.Stop()
}

func (pdi *ProcessDirectoryInput) Run(ir InputRunner, h PluginHelper) (err error) {
	pdi.ir = ir
	pdi.h = h
	if err = pdi.loadInputs(); err != nil {
		return
	}

	var ok = true
	ticker := ir.Ticker()

	for ok {
		select {
		case _, ok = <-pdi.stopChan:
		case <-ticker:
			if err = pdi.loadInputs(); err != nil {
				return
			}
		}
	}

	return
}

// Reload the set of running ProcessInput InputRunners. Not reentrant, should
// only be called from the ProcessDirectoryInput's main Run goroutine.
func (pdi *ProcessDirectoryInput) loadInputs() (err error) {
	// Clear out pdi.specified and then populate it from the file tree.
	pdi.specified = make(map[string]*ProcessEntry)
	if err = filepath.Walk(pdi.procDir, pdi.procDirWalkFunc); err != nil {
		return
	}

	// Remove any running inputs that are no longer specified
	for name, entry := range pdi.inputs {
		if _, ok := pdi.specified[name]; !ok {
			pdi.pConfig.RemoveInputRunner(entry.ir)
			delete(pdi.inputs, name)
			pdi.ir.LogMessage(fmt.Sprintf("Removed: %s", name))
		}
	}

	// Iterate through the specified inputs and activate any that are new or
	// have been modified.
	for name, newEntry := range pdi.specified {

		if runningEntry, ok := pdi.inputs[name]; ok {
			if runningEntry.config.Equals(newEntry.config) {
				// Nothing has changed, let this one keep running.
				continue
			}
			// It has changed, stop the old one.
			pdi.pConfig.RemoveInputRunner(runningEntry.ir)
			pdi.ir.LogMessage(fmt.Sprintf("Removed: %s", name))
			delete(pdi.inputs, name)
		}

		// Start up a new input.
		if err = pdi.pConfig.AddInputRunner(newEntry.ir); err != nil {
			pdi.ir.LogError(fmt.Errorf("creating input '%s': %s", name, err))
			continue
		}
		pdi.inputs[name] = newEntry
		pdi.ir.LogMessage(fmt.Sprintf("Added: %s", name))
	}
	return
}

// Function of type filepath.WalkFunc, called repeatedly when we walk a
// directory tree using filepath.Walk. This function is not reentrant, it
// should only ever be triggered from the similarly not reentrant loadInputs
// method.
func (pdi *ProcessDirectoryInput) procDirWalkFunc(path string, info os.FileInfo,
	err error) error {

	if err != nil {
		pdi.ir.LogError(fmt.Errorf("walking '%s': %s", path, err))
		return nil
	}
	// info == nil => filepath doesn't actually exist.
	if info == nil {
		return nil
	}
	// Skip directories or anything that doesn't end in `.toml`.
	if info.IsDir() || filepath.Ext(path) != ".toml" {
		return nil
	}

	// Make sure that the path doesn't get deeper than one level past
	// procDir. It should match `/procDir/<time_interval>/`.
	dir := filepath.Dir(path)
	parentDir, timeInterval := filepath.Split(dir)
	parentDir = strings.TrimSuffix(parentDir, string(os.PathSeparator))
	if parentDir != pdi.procDir {
		pdi.ir.LogError(fmt.Errorf("invalid ProcessInput path: %s.", path))
		return nil
	}

	// Extract and validate ticker interval from file path.
	var tickInterval int
	if tickInterval, err = strconv.Atoi(timeInterval); err != nil {
		pdi.ir.LogError(fmt.Errorf("ticker interval could not be parsed for '%s'.",
			path))
		return nil
	}
	if tickInterval < 0 {
		pdi.ir.LogError(fmt.Errorf("a negative ticker interval was parsed for '%s'.",
			path))
		return nil
	}

	// Things look good so far. Try to load the data into a config struct.
	var entry *ProcessEntry
	if entry, err = pdi.loadProcessFile(path); err != nil {
		pdi.ir.LogError(fmt.Errorf("loading process file '%s': %s", path, err))
		return nil
	}
	// Override the ticker_interval, make the runner, and store the entry.
	entry.config.TickerInterval = uint(tickInterval)

	runner, err := entry.maker.MakeRunner("")
	if err != nil {
		pdi.ir.LogError(fmt.Errorf("making runner: %s", err.Error()))
		return nil
	}

	entry.ir = runner.(InputRunner)
	entry.ir.SetTransient(true)
	pdi.specified[path] = entry
	return nil
}

func (pdi *ProcessDirectoryInput) loadProcessFile(path string) (*ProcessEntry, error) {
	var err error
	unparsedConfig := make(map[string]toml.Primitive)
	if _, err = toml.DecodeFile(path, &unparsedConfig); err != nil {
		return nil, err
	}
	section, ok := unparsedConfig["ProcessInput"]
	if !ok {
		err = errors.New("No `ProcessInput` section.")
		return nil, err
	}

	maker, err := NewPluginMaker("ProcessInput", pdi.pConfig, section)
	if err != nil {
		return nil, fmt.Errorf("can't create plugin maker: %s", err)
	}

	mutMaker := maker.(MutableMaker)
	mutMaker.SetName(path)
	if err = mutMaker.PrepConfig(); err != nil {
		return nil, fmt.Errorf("can't prep config: %s", err)
	}

	// CanExit is true by default on ProcessInput's we spawn
	commonConfig := mutMaker.CommonTypedConfig().(CommonInputConfig)
	if commonConfig.CanExit == nil {
		b := true
		commonConfig.CanExit = &b
		mutMaker.SetCommonTypedConfig(commonConfig)
	}

	entry := &ProcessEntry{
		maker:  mutMaker,
		config: mutMaker.Config().(*ProcessInputConfig),
	}
	return entry, nil
}

func init() {
	RegisterPlugin("ProcessDirectoryInput", func() interface{} {
		return new(ProcessDirectoryInput)
	})
}
