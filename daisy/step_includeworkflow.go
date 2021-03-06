//  Copyright 2017 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package daisy

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// IncludeWorkflow defines a Daisy workflow injection step. This step will
// 'include' the workflow found the path given into the parent workflow. Unlike
// a Subworkflow the included workflow will exist in the same namespace
// as the parent and have access to all its resources.
type IncludeWorkflow struct {
	Path string
	Vars map[string]string `json:",omitempty"`
	w    *Workflow
}

func (i *IncludeWorkflow) populate(ctx context.Context, s *Step) error {
	i.w.parent = s.w
	i.w.id = s.w.id
	i.w.username = s.w.username
	i.w.ComputeClient = s.w.ComputeClient
	i.w.StorageClient = s.w.StorageClient
	i.w.GCSPath = s.w.GCSPath
	i.w.Name = s.name
	i.w.Project = s.w.Project
	i.w.Zone = s.w.Zone
	i.w.autovars = s.w.autovars
	i.w.bucket = s.w.bucket
	i.w.scratchPath = s.w.scratchPath
	i.w.sourcesPath = s.w.sourcesPath
	i.w.logsPath = s.w.logsPath
	i.w.outsPath = s.w.outsPath
	i.w.gcsLogWriter = s.w.gcsLogWriter
	i.w.gcsLogging = s.w.gcsLogging

	for k, v := range i.Vars {
		i.w.AddVar(k, v)
	}

	var replacements []string
	for k, v := range i.w.autovars {
		if k == "NAME" {
			v = s.name
		}
		if k == "WFDIR" {
			v = i.w.workflowDir
		}
		replacements = append(replacements, fmt.Sprintf("${%s}", k), v)
	}
	for k, v := range i.w.Vars {
		replacements = append(replacements, fmt.Sprintf("${%s}", k), v.Value)
	}
	substitute(reflect.ValueOf(i.w).Elem(), strings.NewReplacer(replacements...))

	i.w.populateLogger(ctx)

	for name, st := range i.w.Steps {
		st.name = name
		st.w = i.w
		if err := st.w.populateStep(ctx, st); err != nil {
			return err
		}
	}

	// Copy Sources up to parent resolving relative paths as we go.
	for k, v := range i.w.Sources {
		if v == "" {
			continue
		}
		if _, ok := s.w.Sources[k]; ok {
			return fmt.Errorf("source %q already exists in workflow", k)
		}
		if s.w.Sources == nil {
			s.w.Sources = map[string]string{}
		}

		if _, _, err := splitGCSPath(v); err != nil && !filepath.IsAbs(v) {
			v = filepath.Join(i.w.workflowDir, v)
		}
		s.w.Sources[k] = v
	}

	return nil
}

func (i *IncludeWorkflow) validate(ctx context.Context, s *Step) error {
	return i.w.validate(ctx)
}

func (i *IncludeWorkflow) run(ctx context.Context, s *Step) error {
	return i.w.run(ctx)
}
