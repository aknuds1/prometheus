// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin || linux

package remote

import (
	"time"

	"golang.org/x/sys/unix"
)

func metadataPipelineCPU() (metadataPipelineCPUUsage, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return metadataPipelineCPUUsage{}, err
	}
	return metadataPipelineCPUUsage{User: time.Duration(usage.Utime.Nano()), System: time.Duration(usage.Stime.Nano()), Available: true}, nil
}
