// Copyright 2025 The Prometheus Authors
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

package semconv

import (
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TODO: Remove me after done testing.
//
//go:embed registry/*
var embeddedRegistry embed.FS

// change represents a change between metric versions.
type change struct {
	Forward  metricGroupChange
	Backward metricGroupChange
}

// metricGroupChange represents a semconv metric group change.
type metricGroupChange struct {
	MetricName  string      `yaml:"metric_name"`
	Unit        string      `yaml:"unit"`
	ValuePromQL string      `yaml:"value_promql"`
	Attributes  []attribute `yaml:"attributes"`
}

func (m metricGroupChange) DirectUnit() string {
	if strings.HasPrefix(m.Unit, "{") {
		return strings.Trim(m.Unit, "{}") + "s"
	}
	return m.Unit
}

type attribute struct {
	Tag     string            `yaml:"tag"`
	Members []attributeMember `yaml:"members"`
}

type attributeMember struct {
	Value string `yaml:"value"`
}

func fetchAndUnmarshal[T any](url string, out *T) error {
	var b []byte
	switch {
	case strings.HasPrefix(url, "http"):
		resp, err := http.Get(url)
		if err != nil {
			return fmt.Errorf("http fetch %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("http fetch %s, got non-200 status: %d", url, resp.StatusCode)
		}

		// TODO(bwplotka): Add limit.
		b, err = io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read from http %s: %w", url, err)
		}
	// TODO: Remove me after testing.
	case strings.HasPrefix(url, "registry/"):
		var err error
		b, err = embeddedRegistry.ReadFile(url)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", url, err)
		}
	default:
		var err error
		b, err = os.ReadFile(url)
		if err != nil {
			return fmt.Errorf("read file %s: %w", url, err)
		}
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unmarshal %q: %w", url, err)
	}
	return nil
}
