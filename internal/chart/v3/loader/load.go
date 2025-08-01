/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package loader

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	chart "helm.sh/helm/v4/internal/chart/v3"
)

// ChartLoader loads a chart.
type ChartLoader interface {
	Load() (*chart.Chart, error)
}

// Loader returns a new ChartLoader appropriate for the given chart name
func Loader(name string) (ChartLoader, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return DirLoader(name), nil
	}
	return FileLoader(name), nil
}

// Load takes a string name, tries to resolve it to a file or directory, and then loads it.
//
// This is the preferred way to load a chart. It will discover the chart encoding
// and hand off to the appropriate chart reader.
//
// If a .helmignore file is present, the directory loader will skip loading any files
// matching it. But .helmignore is not evaluated when reading out of an archive.
func Load(name string) (*chart.Chart, error) {
	l, err := Loader(name)
	if err != nil {
		return nil, err
	}
	return l.Load()
}

// BufferedFile represents an archive file buffered for later processing.
type BufferedFile struct {
	Name string
	Data []byte
}

// LoadFiles loads from in-memory files.
func LoadFiles(files []*BufferedFile) (*chart.Chart, error) {
	c := new(chart.Chart)
	subcharts := make(map[string][]*BufferedFile)

	// do not rely on assumed ordering of files in the chart and crash
	// if Chart.yaml was not coming early enough to initialize metadata
	for _, f := range files {
		c.Raw = append(c.Raw, &chart.File{Name: f.Name, Data: f.Data})
		if f.Name == "Chart.yaml" {
			if c.Metadata == nil {
				c.Metadata = new(chart.Metadata)
			}
			if err := yaml.Unmarshal(f.Data, c.Metadata); err != nil {
				return c, fmt.Errorf("cannot load Chart.yaml: %w", err)
			}
			// While the documentation says the APIVersion is required, in practice there
			// are cases where that's not enforced. Since this package set is for v3 charts,
			// when this function is used v3 is automatically added when not present.
			if c.Metadata.APIVersion == "" {
				c.Metadata.APIVersion = chart.APIVersionV3
			}
		}
	}
	for _, f := range files {
		switch {
		case f.Name == "Chart.yaml":
			// already processed
			continue
		case f.Name == "Chart.lock":
			c.Lock = new(chart.Lock)
			if err := yaml.Unmarshal(f.Data, &c.Lock); err != nil {
				return c, fmt.Errorf("cannot load Chart.lock: %w", err)
			}
		case f.Name == "values.yaml":
			values, err := LoadValues(bytes.NewReader(f.Data))
			if err != nil {
				return c, fmt.Errorf("cannot load values.yaml: %w", err)
			}
			c.Values = values
		case f.Name == "values.schema.json":
			c.Schema = f.Data

		case strings.HasPrefix(f.Name, "templates/"):
			c.Templates = append(c.Templates, &chart.File{Name: f.Name, Data: f.Data})
		case strings.HasPrefix(f.Name, "charts/"):
			if filepath.Ext(f.Name) == ".prov" {
				c.Files = append(c.Files, &chart.File{Name: f.Name, Data: f.Data})
				continue
			}

			fname := strings.TrimPrefix(f.Name, "charts/")
			cname := strings.SplitN(fname, "/", 2)[0]
			subcharts[cname] = append(subcharts[cname], &BufferedFile{Name: fname, Data: f.Data})
		default:
			c.Files = append(c.Files, &chart.File{Name: f.Name, Data: f.Data})
		}
	}

	if c.Metadata == nil {
		return c, errors.New("Chart.yaml file is missing") //nolint:staticcheck
	}

	if err := c.Validate(); err != nil {
		return c, err
	}

	for n, files := range subcharts {
		var sc *chart.Chart
		var err error
		switch {
		case strings.IndexAny(n, "_.") == 0:
			continue
		case filepath.Ext(n) == ".tgz":
			file := files[0]
			if file.Name != n {
				return c, fmt.Errorf("error unpacking subchart tar in %s: expected %s, got %s", c.Name(), n, file.Name)
			}
			// Untar the chart and add to c.Dependencies
			sc, err = LoadArchive(bytes.NewBuffer(file.Data))
		default:
			// We have to trim the prefix off of every file, and ignore any file
			// that is in charts/, but isn't actually a chart.
			buff := make([]*BufferedFile, 0, len(files))
			for _, f := range files {
				parts := strings.SplitN(f.Name, "/", 2)
				if len(parts) < 2 {
					continue
				}
				f.Name = parts[1]
				buff = append(buff, f)
			}
			sc, err = LoadFiles(buff)
		}

		if err != nil {
			return c, fmt.Errorf("error unpacking subchart %s in %s: %w", n, c.Name(), err)
		}
		c.AddDependency(sc)
	}

	return c, nil
}

// LoadValues loads values from a reader.
//
// The reader is expected to contain one or more YAML documents, the values of which are merged.
// And the values can be either a chart's default values or a user-supplied values.
func LoadValues(data io.Reader) (map[string]interface{}, error) {
	values := map[string]interface{}{}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(data))
	for {
		currentMap := map[string]interface{}{}
		raw, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error reading yaml document: %w", err)
		}
		if err := yaml.Unmarshal(raw, &currentMap); err != nil {
			return nil, fmt.Errorf("cannot unmarshal yaml document: %w", err)
		}
		values = MergeMaps(values, currentMap)
	}
	return values, nil
}

// MergeMaps merges two maps. If a key exists in both maps, the value from b will be used.
// If the value is a map, the maps will be merged recursively.
func MergeMaps(a, b map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(a))
	maps.Copy(out, a)
	for k, v := range b {
		if v, ok := v.(map[string]interface{}); ok {
			if bv, ok := out[k]; ok {
				if bv, ok := bv.(map[string]interface{}); ok {
					out[k] = MergeMaps(bv, v)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}
