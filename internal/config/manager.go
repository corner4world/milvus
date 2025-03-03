// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package config

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/milvus-io/milvus/internal/log"
	"go.uber.org/zap"
)

const (
	TombValue = "TOMB_VAULE"
)

type Filter func(key string) (string, bool)

func WithSubstr(substring string) Filter {
	substring = strings.ToLower(substring)
	return func(key string) (string, bool) {
		return key, strings.Contains(key, substring)
	}
}

func WithPrefix(prefix string) Filter {
	prefix = strings.ToLower(prefix)
	return func(key string) (string, bool) {
		return key, strings.HasPrefix(key, prefix)
	}
}

func WithOneOfPrefixs(prefixs ...string) Filter {
	for id, prefix := range prefixs {
		prefixs[id] = strings.ToLower(prefix)
	}
	return func(key string) (string, bool) {
		for _, prefix := range prefixs {
			if strings.HasPrefix(key, prefix) {
				return key, true
			}
		}
		return key, false
	}
}

func RemovePrefix(prefix string) Filter {
	prefix = strings.ToLower(prefix)
	return func(key string) (string, bool) {
		return strings.Replace(key, prefix, "", 1), true
	}
}

func filterate(key string, filters ...Filter) (string, bool) {
	var ok bool
	for _, filter := range filters {
		key, ok = filter(key)
		if !ok {
			return key, ok
		}
	}
	return key, ok
}

type Manager struct {
	sync.RWMutex
	Dispatcher     *EventDispatcher
	sources        map[string]Source
	keySourceMap   map[string]string
	overlayConfigs map[string]string
}

func NewManager() *Manager {
	return &Manager{
		Dispatcher:     NewEventDispatcher(),
		sources:        make(map[string]Source),
		keySourceMap:   make(map[string]string),
		overlayConfigs: make(map[string]string),
	}
}

func (m *Manager) GetConfig(key string) (string, error) {
	m.RLock()
	defer m.RUnlock()
	realKey := formatKey(key)
	v, ok := m.overlayConfigs[realKey]
	if ok {
		if v == TombValue {
			return "", fmt.Errorf("key not found %s", key)
		}
		return v, nil
	}
	sourceName, ok := m.keySourceMap[realKey]
	if !ok {
		return "", fmt.Errorf("key not found: %s", key)
	}
	return m.getConfigValueBySource(realKey, sourceName)
}

func (m *Manager) GetBy(filters ...Filter) map[string]string {
	m.RLock()
	defer m.RUnlock()
	matchedConfig := make(map[string]string)

	for key, value := range m.keySourceMap {
		newkey, ok := filterate(key, filters...)
		if ok {
			sValue, err := m.getConfigValueBySource(key, value)
			if err != nil {
				log.Error("Get some invalid config", zap.String("key", key))
				continue
			}
			matchedConfig[newkey] = sValue
		}
	}

	return matchedConfig
}

// GetConfigs returns all the key values
func (m *Manager) GetConfigs() map[string]string {
	m.RLock()
	defer m.RUnlock()
	config := make(map[string]string)

	for key, value := range m.keySourceMap {
		sValue, err := m.getConfigValueBySource(key, value)
		if err != nil {
			continue
		}
		config[key] = sValue
	}

	return config
}

func (m *Manager) Close() {
	for _, s := range m.sources {
		s.Close()
	}
}

func (m *Manager) AddSource(source Source) error {
	m.Lock()
	defer m.Unlock()
	sourceName := source.GetSourceName()
	_, ok := m.sources[sourceName]
	if ok {
		err := errors.New("duplicate source supplied")
		return err
	}

	m.sources[sourceName] = source

	err := m.pullSourceConfigs(sourceName)
	if err != nil {
		err = fmt.Errorf("failed to load %s cause: %x", sourceName, err)
		return err
	}

	source.SetEventHandler(m)

	return nil
}

// For compatible reason, only visiable for Test
func (m *Manager) SetConfig(key, value string) {
	m.Lock()
	defer m.Unlock()
	m.overlayConfigs[formatKey(key)] = value
}

// For compatible reason, only visiable for Test
func (m *Manager) DeleteConfig(key string) {
	m.Lock()
	defer m.Unlock()
	m.overlayConfigs[formatKey(key)] = TombValue
}

func (m *Manager) ResetConfig(key string) {
	m.Lock()
	defer m.Unlock()
	delete(m.overlayConfigs, formatKey(key))
}

// Do not use it directly, only used when add source and unittests.
func (m *Manager) pullSourceConfigs(source string) error {
	configSource, ok := m.sources[source]
	if !ok {
		return errors.New("invalid source or source not added")
	}

	configs, err := configSource.GetConfigurations()
	if err != nil {
		log.Error("Get configuration by items failed", zap.Error(err))
		return err
	}

	sourcePriority := configSource.GetPriority()
	for key := range configs {
		sourceName, ok := m.keySourceMap[key]
		if !ok { // if key do not exist then add source
			m.keySourceMap[key] = source
			continue
		}

		currentSource, ok := m.sources[sourceName]
		if !ok {
			m.keySourceMap[key] = source
			continue
		}

		currentSrcPriority := currentSource.GetPriority()
		if currentSrcPriority > sourcePriority { // lesser value has high priority
			m.keySourceMap[key] = source
		}
	}

	return nil
}

func (m *Manager) getConfigValueBySource(configKey, sourceName string) (string, error) {
	source, ok := m.sources[sourceName]
	if !ok {
		return "", ErrKeyNotFound
	}

	return source.GetConfigurationByKey(configKey)
}

func (m *Manager) updateEvent(e *Event) error {
	// refresh all configuration one by one
	log.Debug("receive update event", zap.Any("event", e))
	if e.HasUpdated {
		return nil
	}
	switch e.EventType {
	case CreateType, UpdateType:
		sourceName, ok := m.keySourceMap[e.Key]
		if !ok {
			m.keySourceMap[e.Key] = e.EventSource
			e.EventType = CreateType
		} else if sourceName == e.EventSource {
			e.EventType = UpdateType
		} else if sourceName != e.EventSource {
			prioritySrc := m.getHighPrioritySource(sourceName, e.EventSource)
			if prioritySrc != nil && prioritySrc.GetSourceName() == sourceName {
				// if event generated from less priority source then ignore
				log.Info(fmt.Sprintf("the event source %s's priority is less then %s's, ignore",
					e.EventSource, sourceName))
				return ErrIgnoreChange
			}
			m.keySourceMap[e.Key] = e.EventSource
			e.EventType = UpdateType
		}

	case DeleteType:
		sourceName, ok := m.keySourceMap[e.Key]
		if !ok || sourceName != e.EventSource {
			// if delete event generated from source not maintained ignore it
			log.Info(fmt.Sprintf("the event source %s (expect %s) is not maintained, ignore",
				e.EventSource, sourceName))
			return ErrIgnoreChange
		} else if sourceName == e.EventSource {
			// find less priority source or delete key
			source := m.findNextBestSource(e.Key, sourceName)
			if source == nil {
				delete(m.keySourceMap, e.Key)
			} else {
				m.keySourceMap[e.Key] = source.GetSourceName()
			}
		}

	}

	e.HasUpdated = true
	return nil
}

// OnEvent Triggers actions when an event is generated
func (m *Manager) OnEvent(event *Event) {
	m.Lock()
	defer m.Unlock()
	err := m.updateEvent(event)
	if err != nil {
		log.Warn("failed in updating event with error", zap.Error(err), zap.Any("event", event))
		return
	}

	m.Dispatcher.Dispatch(event)
}

func (m *Manager) GetIdentifier() string {
	return "Manager"
}

func (m *Manager) findNextBestSource(key string, sourceName string) Source {
	var rSource Source
	for _, source := range m.sources {
		if source.GetSourceName() == sourceName {
			continue
		}
		_, err := source.GetConfigurationByKey(key)
		if err != nil {
			continue
		}
		if rSource == nil {
			rSource = source
			continue
		}
		if source.GetPriority() < rSource.GetPriority() { // less value has high priority
			rSource = source
		}
	}

	return rSource
}

func (m *Manager) getHighPrioritySource(srcNameA, srcNameB string) Source {
	sourceA, okA := m.sources[srcNameA]
	sourceB, okB := m.sources[srcNameB]

	if !okA && !okB {
		return nil
	} else if !okA {
		return sourceB
	} else if !okB {
		return sourceA
	}

	if sourceA.GetPriority() < sourceB.GetPriority() { //less value has high priority
		return sourceA
	}

	return sourceB
}
