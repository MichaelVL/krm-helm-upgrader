// Copyright 2023 Michael Vittrup Larsen
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleContainerTools/kpt-functions-sdk/go/fn"
	securejoin "github.com/cyphar/filepath-securejoin"
	t "github.com/michaelvl/krm-functions/pkg/helmspecs"
	"sigs.k8s.io/kustomize/kyaml/kio"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
)

const (
	maxChartTemplateFileLength = 1024 * 1024
)

// Template extracts a chart tarball and renders the chart using given
// values and `helm template`. The raw chart tarball data is given in
// `chartTarball` (note, not base64 encoded). Returns the rendered
// objects
func Template(chart *t.HelmChart, chartTarball []byte) (fn.KubeObjects, error) {
	tmpDir, err := os.MkdirTemp("", "chart-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	gzr, err := gzip.NewReader(bytes.NewReader(chartTarball))
	if err != nil {
		return nil, err
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	// Extract tar archive files
	for {
		hdr, xtErr := tr.Next()
		if xtErr == io.EOF {
			break // End of archive
		} else if xtErr != nil {
			return nil, xtErr
		}
		fname := hdr.Name
		if path.IsAbs(fname) {
			return nil, errors.New("chart contains file with absolute path")
		}
		fileWithPath, fnerr := securejoin.SecureJoin(tmpDir, fname)
		if fnerr != nil {
			return nil, fnerr
		}
		if hdr.Typeflag == tar.TypeReg {
			fdir := filepath.Dir(fileWithPath)
			if mkdErr := os.MkdirAll(fdir, 0o755); mkdErr != nil {
				return nil, mkdErr
			}

			file, fErr := os.OpenFile(fileWithPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if fErr != nil {
				return nil, fErr
			}
			_, fErr = io.CopyN(file, tr, maxChartTemplateFileLength)
			file.Close()
			if fErr != nil && fErr != io.EOF {
				return nil, fErr
			}
		}
	}

	valuesFile := filepath.Join(tmpDir, "values.yaml")
	err = writeValuesFile(chart, valuesFile)
	if err != nil {
		return nil, err
	}
	args := buildHelmTemplateArgs(chart)
	args = append(args, "--values", valuesFile, filepath.Join(tmpDir, chart.Args.Name))

	helmCtxt := NewRunContext()
	defer helmCtxt.DiscardContext()
	stdout, err := helmCtxt.Run(args...)
	if err != nil {
		return nil, err
	}

	r := &kio.ByteReader{Reader: bytes.NewBufferString(string(stdout)), OmitReaderAnnotations: true}
	nodes, err := r.Read()
	if err != nil {
		return nil, err
	}

	var objects fn.KubeObjects
	for i := range nodes {
		o, parseErr := fn.ParseKubeObject([]byte(nodes[i].MustString()))
		if parseErr != nil {
			if strings.Contains(parseErr.Error(), "expected exactly one object, got 0") {
				continue
			}
			return nil, fmt.Errorf("failed to parse %s: %s", nodes[i].MustString(), parseErr.Error())
		}
		objects = append(objects, o)
	}

	if err != nil {
		return nil, err
	}

	return objects, nil
}

// writeValuesFile writes chart values to a file for passing to Helm
func writeValuesFile(chart *t.HelmChart, valuesFilename string) error {
	vals := chart.Options.Values.ValuesInline
	b, err := kyaml.Marshal(vals)
	if err != nil {
		return err
	}
	return os.WriteFile(valuesFilename, b, 0o600)
}

// buildHelmTemplateArgs prepares arguments for `helm template`
func buildHelmTemplateArgs(chart *t.HelmChart) []string {
	opts := chart.Options
	args := []string{"template"}
	if opts.ReleaseName != "" {
		args = append(args, opts.ReleaseName)
	}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
	}
	if opts.NameTemplate != "" {
		args = append(args, "--name-template", opts.NameTemplate)
	}
	for _, apiVer := range opts.APIVersions {
		args = append(args, "--api-versions", apiVer)
	}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	if opts.IncludeCRDs {
		args = append(args, "--include-crds")
	}
	if opts.SkipTests {
		args = append(args, "--skip-tests")
	}
	return args
}
