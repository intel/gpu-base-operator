/*
Copyright 2026 Intel Corporation. All Rights Reserved.

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

package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// outputFormat is a -o/--output value. The plugin implements the subset of kubectl's formats its
// own output has a meaning for: the default table, the same table with the columns a narrow
// terminal cannot fit, and the raw API data behind the table.
type outputFormat string

const (
	outputTable outputFormat = ""
	outputWide  outputFormat = "wide"
	outputYAML  outputFormat = "yaml"
)

// outputFormats are the accepted -o values, in the order the flag's error message and its shell
// completion list them.
var outputFormats = []string{"wide", "yaml"}

// parseOutputFormat validates a raw -o value. An empty value is the default table, so that an
// empty -o argument behaves like omitting the flag rather than failing.
func parseOutputFormat(s string) (outputFormat, error) {
	switch f := outputFormat(s); f {
	case outputTable, outputWide, outputYAML:
		return f, nil
	default:
		return outputTable, fmt.Errorf("unsupported output format %q: must be one of %s",
			s, strings.Join(outputFormats, ", "))
	}
}

// addOutputFlag registers -o/--output on cmd together with the completion for its values.
func addOutputFlag(cmd *cobra.Command, target *string, usage string) {
	cmd.Flags().StringVarP(target, "output", "o", "", usage)

	// nolint:errcheck
	cmd.RegisterFlagCompletionFunc("output", completeStaticValues(outputFormats))
}

// printYAML writes v as YAML to out. v must hold only the JSON-compatible types the dynamic
// client produces (map[string]interface{}, []interface{}, string, bool, int64, float64), which
// is what sigs.k8s.io/yaml can round-trip through JSON.
func printYAML(out io.Writer, v interface{}) error {
	buf, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling YAML: %w", err)
	}

	_, err = out.Write(buf)

	return err
}
