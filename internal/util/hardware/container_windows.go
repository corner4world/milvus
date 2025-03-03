// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package hardware

import (
	"errors"
)

// inContainer checks if the service is running inside a container
// It should be always false while under windows.
func inContainer() (bool, error) {
	return false, nil
}

// getContainerMemLimit returns memory limit and error
func getContainerMemLimit() (uint64, error) {
	return 0, errors.New("Not supported")
}

// getContainerMemUsed returns memory usage and error
func getContainerMemUsed() (uint64, error) {
	return 0, errors.New("Not supported")
}
