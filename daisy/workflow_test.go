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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/kylelemons/godebug/diff"
	"github.com/kylelemons/godebug/pretty"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

func TestAddDependency(t *testing.T) {
	w := &Workflow{Steps: map[string]*Step{"a": nil, "b": nil}}

	tests := []struct {
		desc      string
		in1, in2  string
		shouldErr bool
	}{
		{"good case", "a", "b", false},
		{"idempotent good case", "a", "b", false},
		{"bad case 1", "a", "c", true},
		{"bad case 2", "c", "b", true},
	}

	for _, tt := range tests {
		if err := w.AddDependency(tt.in1, tt.in2); err == nil && tt.shouldErr {
			t.Errorf("%s: should have erred but didn't", tt.desc)
		} else if err != nil && !tt.shouldErr {
			t.Errorf("%s: unexpected error: %v", tt.desc, err)
		}
	}

	wantDeps := map[string][]string{"a": {"b"}}
	if diff := pretty.Compare(w.Dependencies, wantDeps); diff != "" {
		t.Errorf("incorrect dependencies: (-got,+want)\n%s", diff)
	}
}

func TestDaisyBkt(t *testing.T) {
	client, err := newTestGCSClient()
	if err != nil {
		t.Fatal(err)
	}
	project := "foo-project"
	got, err := daisyBkt(context.Background(), client, project)
	if err != nil {
		t.Fatal(err)
	}
	want := project + "-daisy-bkt"
	if got != project+"-daisy-bkt" {
		t.Errorf("bucket does not match, got: %q, want: %q", got, want)
	}

	project = "bar-project"
	got, err = daisyBkt(context.Background(), client, project)
	if err != nil {
		t.Fatal(err)
	}
	want = project + "-daisy-bkt"
	if got != project+"-daisy-bkt" {
		t.Errorf("bucket does not match, got: %q, want: %q", got, want)
	}
}

func TestCleanup(t *testing.T) {
	cleanedup1 := false
	cleanedup2 := false
	cleanup1 := func() error {
		cleanedup1 = true
		return nil
	}
	cleanup2 := func() error {
		cleanedup2 = true
		return nil
	}
	cleanupFail := func() error {
		return errors.New("failed cleanup")
	}

	w := testWorkflow()
	w.addCleanupHook(cleanup1)
	w.addCleanupHook(cleanupFail)
	w.addCleanupHook(cleanup2)
	w.cleanup()

	if !cleanedup1 {
		t.Error("cleanup1 was not run")
	}
	if !cleanedup2 {
		t.Error("cleanup2 was not run")
	}
}

func TestGenName(t *testing.T) {
	tests := []struct{ name, wfName, wfID, want string }{
		{"name", "wfname", "123456789", "name-wfname-123456789"},
		{"super-long-name-really-long", "super-long-workflow-name-like-really-really-long", "1", "super-long-name-really-long-super-long-workflow-name-lik-1"},
		{"super-long-name-really-long", "super-long-workflow-name-like-really-really-long", "123456789", "super-long-name-really-long-super-long-workflow-name-lik-123456"},
	}
	w := &Workflow{}
	for _, tt := range tests {
		w.id = tt.wfID
		w.Name = tt.wfName
		result := w.genName(tt.name)
		if result != tt.want {
			t.Errorf("bad result, input: name=%s wfName=%s wfId=%s; got: %s; want: %s", tt.name, tt.wfName, tt.wfID, result, tt.want)
		}
		if len(result) > 64 {
			t.Errorf("result > 64 characters, input: name=%s wfName=%s wfId=%s; got: %s", tt.name, tt.wfName, tt.wfID, result)
		}
	}
}

func TestGetSourceGCSAPIPath(t *testing.T) {
	w := testWorkflow()
	w.sourcesPath = "my/sources"
	got := w.getSourceGCSAPIPath("foo")
	want := "https://storage.cloud.google.com/my/sources/foo"
	if got != want {
		t.Errorf("unexpected result: got: %q, want %q", got, want)
	}
}

func TestNewFromFileError(t *testing.T) {
	td, err := ioutil.TempDir(os.TempDir(), "")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	defer os.RemoveAll(td)
	tf := filepath.Join(td, "test.wf.json")

	localDNEErr := "open %s/sub.workflow: no such file or directory"
	if runtime.GOOS == "windows" {
		localDNEErr = "open %s\\sub.workflow: The system cannot find the file specified."
	}
	tests := []struct{ data, error string }{
		{
			`{"test":["1", "2",]}`,
			tf + ": JSON syntax error in line 1: invalid character ']' looking for beginning of value \n{\"test\":[\"1\", \"2\",]}\n                  ^",
		},
		{
			`{"test":{"key1":"value1" "key2":"value2"}}`,
			tf + ": JSON syntax error in line 1: invalid character '\"' after object key:value pair \n{\"test\":{\"key1\":\"value1\" \"key2\":\"value2\"}}\n                         ^",
		},
		{
			`{"test": value}`,
			tf + ": JSON syntax error in line 1: invalid character 'v' looking for beginning of value \n{\"test\": value}\n         ^",
		},
		{
			`{"test": "value"`,
			tf + ": JSON syntax error in line 1: unexpected end of JSON input \n{\"test\": \"value\"\n               ^",
		},
		{
			"{\n\"test\":[\"1\", \"2\",],\n\"test2\":[\"1\", \"2\"]\n}",
			tf + ": JSON syntax error in line 2: invalid character ']' looking for beginning of value \n\"test\":[\"1\", \"2\",],\n                 ^",
		},
		{
			`{"steps": {"somename": {"subWorkflow": {"path": "sub.workflow"}}}}`,
			fmt.Sprintf(localDNEErr, td),
		},
	}

	for _, tt := range tests {
		if err := ioutil.WriteFile(tf, []byte(tt.data), 0600); err != nil {
			t.Fatalf("error creating json file: %v", err)
		}

		if _, err := NewFromFile(tf); err == nil {
			t.Error("expected error, got nil")
		} else if err.Error() != tt.error {
			t.Errorf("did not get expected error from NewFromFile():\ngot: %q\nwant: %q", err.Error(), tt.error)
		}
	}
}

func TestNewFromFile(t *testing.T) {
	got, err := NewFromFile("./test_data/test.wf.json")
	if err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	subGot := got.Steps["sub-workflow"].SubWorkflow.w
	includeGot := got.Steps["include-workflow"].IncludeWorkflow.w

	want := &Workflow{
		id:          got.id,
		workflowDir: filepath.Join(wd, "test_data"),
		Name:        "some-name",
		Project:     "some-project",
		Zone:        "us-central1-a",
		GCSPath:     "gs://some-bucket/images",
		OAuthPath:   filepath.Join(wd, "test_data", "somefile"),
		Vars: map[string]vars{
			"bootstrap_instance_name": {Value: "bootstrap-${NAME}", Required: true},
			"machine_type":            {Value: "n1-standard-1"},
		},
		Steps: map[string]*Step{
			"create-disks": {
				name: "create-disks",
				CreateDisks: &CreateDisks{
					{
						Disk: compute.Disk{
							Name:        "bootstrap",
							SourceImage: "projects/windows-cloud/global/images/family/windows-server-2016-core",
							Type:        "pd-ssd",
						},
						SizeGb: "50",
					},
					{
						Disk: compute.Disk{
							Name:        "image",
							SourceImage: "projects/windows-cloud/global/images/family/windows-server-2016-core",
							Type:        "pd-standard",
						},
						SizeGb: "50",
					},
				},
			},
			"${bootstrap_instance_name}": {
				name: "${bootstrap_instance_name}",
				CreateInstances: &CreateInstances{
					{
						Instance: compute.Instance{
							Name:        "${bootstrap_instance_name}",
							Disks:       []*compute.AttachedDisk{{Source: "bootstrap"}, {Source: "image"}},
							MachineType: "${machine_type}",
						},
						StartupScript: "shutdown /h",
						Metadata:      map[string]string{"test_metadata": "this was a test"},
					},
				},
			},
			"${bootstrap_instance_name}-stopped": {
				name:                   "${bootstrap_instance_name}-stopped",
				Timeout:                "1h",
				WaitForInstancesSignal: &WaitForInstancesSignal{{Name: "${bootstrap_instance_name}", Stopped: true, Interval: "1s"}},
			},
			"postinstall": {
				name: "postinstall",
				CreateInstances: &CreateInstances{
					{
						Instance: compute.Instance{
							Name:        "postinstall",
							Disks:       []*compute.AttachedDisk{{Source: "image"}, {Source: "bootstrap"}},
							MachineType: "${machine_type}",
						},
						StartupScript: "shutdown /h",
					},
				},
			},
			"postinstall-stopped": {
				name: "postinstall-stopped",
				WaitForInstancesSignal: &WaitForInstancesSignal{{Name: "postinstall", Stopped: true}},
			},
			"create-image": {
				name:         "create-image",
				CreateImages: &CreateImages{{Image: compute.Image{Name: "image-from-disk", SourceDisk: "image"}}},
			},
			"include-workflow": {
				name: "include-workflow",
				IncludeWorkflow: &IncludeWorkflow{
					Vars: map[string]string{
						"key": "value",
					},
					Path: "./test_sub.wf.json",
					w: &Workflow{
						id:          subGot.id,
						workflowDir: filepath.Join(wd, "test_data"),
						Steps: map[string]*Step{
							"create-disks": {
								name: "create-disks",
								CreateDisks: &CreateDisks{
									{
										Disk: compute.Disk{
											Name:        "bootstrap",
											SourceImage: "projects/windows-cloud/global/images/family/windows-server-2016-core",
										},
										SizeGb: "50",
									},
								},
							},
							"bootstrap": {
								name: "bootstrap",
								CreateInstances: &CreateInstances{
									{
										Instance: compute.Instance{
											Name:        "bootstrap",
											Disks:       []*compute.AttachedDisk{{Source: "bootstrap"}},
											MachineType: "n1-standard-1",
										},
										StartupScript: "shutdown /h",
										Metadata:      map[string]string{"test_metadata": "${key}"},
									},
								},
							},
							"bootstrap-stopped": {
								name:    "bootstrap-stopped",
								Timeout: "1h",
								WaitForInstancesSignal: &WaitForInstancesSignal{
									{
										Name: "bootstrap",
										SerialOutput: &SerialOutput{
											Port: 1, SuccessMatch: "complete", FailureMatch: "fail",
										},
									},
								},
							},
						},
						Dependencies: map[string][]string{
							"bootstrap":         {"create-disks"},
							"bootstrap-stopped": {"bootstrap"},
						},
					},
				},
			},
			"sub-workflow": {
				name: "sub-workflow",
				SubWorkflow: &SubWorkflow{
					Vars: map[string]string{
						"key": "value",
					},
					Path: "./test_sub.wf.json",
					w: &Workflow{
						id:          subGot.id,
						workflowDir: filepath.Join(wd, "test_data"),
						Steps: map[string]*Step{
							"create-disks": {
								name: "create-disks",
								CreateDisks: &CreateDisks{
									{
										Disk: compute.Disk{
											Name:        "bootstrap",
											SourceImage: "projects/windows-cloud/global/images/family/windows-server-2016-core",
										},
										SizeGb: "50",
									},
								},
							},
							"bootstrap": {
								name: "bootstrap",
								CreateInstances: &CreateInstances{
									{
										Instance: compute.Instance{
											Name:        "bootstrap",
											Disks:       []*compute.AttachedDisk{{Source: "bootstrap"}},
											MachineType: "n1-standard-1",
										},
										StartupScript: "shutdown /h",
										Metadata:      map[string]string{"test_metadata": "${key}"},
									},
								},
							},
							"bootstrap-stopped": {
								name:    "bootstrap-stopped",
								Timeout: "1h",
								WaitForInstancesSignal: &WaitForInstancesSignal{
									{
										Name: "bootstrap",
										SerialOutput: &SerialOutput{
											Port: 1, SuccessMatch: "complete", FailureMatch: "fail",
										},
									},
								},
							},
						},
						Dependencies: map[string][]string{
							"bootstrap":         {"create-disks"},
							"bootstrap-stopped": {"bootstrap"},
						},
					},
				},
			},
		},
		Dependencies: map[string][]string{
			"create-disks":        {},
			"bootstrap":           {"create-disks"},
			"bootstrap-stopped":   {"bootstrap"},
			"postinstall":         {"bootstrap-stopped"},
			"postinstall-stopped": {"postinstall"},
			"create-image":        {"postinstall-stopped"},
			"include-workflow":    {"create-image"},
			"sub-workflow":        {"create-image"},
		},
	}

	// Check that subworkflow has workflow as parent.
	if subGot.parent != got {
		t.Error("subworkflow does not point to parent workflow")
	}

	// Fix pretty.Compare recursion freak outs.
	got.Cancel = nil
	for _, s := range got.Steps {
		s.w = nil
	}
	subGot.Cancel = nil
	subGot.parent = nil
	for _, s := range subGot.Steps {
		s.w = nil
	}

	includeGot.Cancel = nil
	includeGot.parent = nil
	for _, s := range includeGot.Steps {
		s.w = nil
	}

	// Cleanup hooks are impossible to check right now.
	got.cleanupHooks = nil
	subGot.cleanupHooks = nil
	includeGot.cleanupHooks = nil

	if diff := pretty.Compare(got, want); diff != "" {
		t.Errorf("parsed workflow does not match expectation: (-got +want)\n%s", diff)
	}
}

func TestNewStep(t *testing.T) {
	w := &Workflow{}

	s, err := w.NewStep("s")
	wantS := &Step{name: "s", w: w}
	if s == nil || s.name != "s" || s.w != w {
		t.Errorf("step does not meet expectation: got: %v, want: %v", s, wantS)
	}
	if err != nil {
		t.Error("unexpected error when creating new step")
	}

	s, err = w.NewStep("s")
	if s != nil {
		t.Errorf("step should not have been created: %v", s)
	}
	if err == nil {
		t.Error("should have erred, but didn't")
	}
}

func TestPopulate(t *testing.T) {
	ctx := context.Background()
	client, err := newTestGCSClient()
	if err != nil {
		t.Fatal(err)
	}
	td, err := ioutil.TempDir(os.TempDir(), "")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	defer os.RemoveAll(td)
	tf := filepath.Join(td, "test.cred")
	if err := ioutil.WriteFile(tf, []byte(`{ "type": "service_account" }`), 0600); err != nil {
		t.Fatalf("error creating temp file: %v", err)
	}

	called := false
	var stepPopErr error
	stepPop := func(ctx context.Context, s *Step) error {
		called = true
		return stepPopErr
	}

	got := New()
	got.Name = "${wf_name}"
	got.Zone = "wf-zone"
	got.Project = "bar-project"
	got.OAuthPath = tf
	got.logger = log.New(ioutil.Discard, "", 0)
	got.Vars = map[string]vars{
		"bucket":    {Value: "wf-bucket", Required: true},
		"step_name": {Value: "step1"},
		"timeout":   {Value: "60m"},
		"path":      {Value: "./test_sub.wf.json"},
		"wf_name":   {Value: "wf-name"},
		"test-var":  {Value: "${ZONE}-this-should-populate-${NAME}"},
	}
	got.Steps = map[string]*Step{
		"${NAME}-${step_name}": {
			w:       got,
			Timeout: "${timeout}",
			testType: &mockStep{
				populateImpl: stepPop,
			},
		},
	}
	got.StorageClient = client
	got.gcsLogging = true

	if err := got.populate(ctx); err != nil {
		t.Fatalf("error populating workflow: %v", err)
	}

	want := &Workflow{
		Name:       "wf-name",
		GCSPath:    "gs://bar-project-daisy-bkt",
		Zone:       "wf-zone",
		Project:    "bar-project",
		OAuthPath:  tf,
		id:         got.id,
		gcsLogging: true,
		Cancel:     got.Cancel,
		Vars: map[string]vars{
			"bucket":    {Value: "wf-bucket", Required: true},
			"step_name": {Value: "step1"},
			"timeout":   {Value: "60m"},
			"path":      {Value: "./test_sub.wf.json"},
			"wf_name":   {Value: "wf-name"},
			"test-var":  {Value: "wf-zone-this-should-populate-wf-name"},
		},
		autovars:    got.autovars,
		bucket:      "bar-project-daisy-bkt",
		scratchPath: got.scratchPath,
		sourcesPath: fmt.Sprintf("%s/sources", got.scratchPath),
		logsPath:    fmt.Sprintf("%s/logs", got.scratchPath),
		outsPath:    fmt.Sprintf("%s/outs", got.scratchPath),
		username:    got.username,
		Steps: map[string]*Step{
			"wf-name-step1": {
				name:    "wf-name-step1",
				Timeout: "60m",
				timeout: time.Duration(60 * time.Minute),
				testType: &mockStep{
					populateImpl: stepPop,
				},
			},
		},
	}

	// Some things to override before checking equivalence:
	// - recursive stuff that breaks pretty.Compare (ComputeClient, StorageClient, Step.w)
	// - stuff that is irrelevant and difficult to check (cleanupHooks and logger)
	for _, wf := range []*Workflow{got, want} {
		wf.ComputeClient = nil
		wf.StorageClient = nil
		wf.logger = nil
		wf.cleanupHooks = nil
		for _, s := range wf.Steps {
			s.w = nil
		}
	}

	if diff := pretty.Compare(got, want); diff != "" {
		t.Errorf("parsed workflow does not match expectation: (-got +want)\n%s", diff)
	}

	if !called {
		t.Error("did not call step's populate")
	}

	stepPopErr = errors.New("error")
	if err := got.populate(ctx); err != stepPopErr {
		t.Errorf("did not get proper step populate error: %v != %v", err, stepPopErr)
	}
}

func testTraverseWorkflow(mockRun func(i int) func(context.Context, *Step) error) *Workflow {
	// s0---->s1---->s3
	//   \         /
	//    --->s2---
	// s4
	w := testWorkflow()
	w.Steps = map[string]*Step{
		"s0": {name: "s0", testType: &mockStep{runImpl: mockRun(0)}, w: w},
		"s1": {name: "s1", testType: &mockStep{runImpl: mockRun(1)}, w: w},
		"s2": {name: "s2", testType: &mockStep{runImpl: mockRun(2)}, w: w},
		"s3": {name: "s3", testType: &mockStep{runImpl: mockRun(3)}, w: w},
		"s4": {name: "s4", testType: &mockStep{runImpl: mockRun(4)}, w: w},
	}
	w.Dependencies = map[string][]string{
		"s1": {"s0"},
		"s2": {"s0"},
		"s3": {"s1", "s2"},
	}
	return w
}

func TestTraverseDAG(t *testing.T) {
	ctx := context.Background()
	var callOrder []int
	errs := make([]error, 5)
	var rw sync.Mutex
	mockRun := func(i int) func(context.Context, *Step) error {
		return func(_ context.Context, _ *Step) error {
			rw.Lock()
			defer rw.Unlock()
			callOrder = append(callOrder, i)
			return errs[i]
		}
	}

	// Check call order: s1 and s2 must be after s0, s3 must be after s1 and s2.
	checkCallOrder := func() error {
		rw.Lock()
		defer rw.Unlock()
		stepOrderNum := []int{-1, -1, -1, -1, -1}
		for i, stepNum := range callOrder {
			stepOrderNum[stepNum] = i
		}
		// If s1 was called, check it was called after s0.
		if stepOrderNum[1] != -1 && stepOrderNum[1] < stepOrderNum[0] {
			return errors.New("s1 was called before s0")
		}
		// If s2 was called, check it was called after s0.
		if stepOrderNum[2] != -1 && stepOrderNum[2] < stepOrderNum[0] {
			return errors.New("s2 was called before s0")
		}
		// If s3 was called, check it was called after s1 and s2.
		if stepOrderNum[3] != -1 {
			if stepOrderNum[3] < stepOrderNum[1] {
				return errors.New("s3 was called before s1")
			}
			if stepOrderNum[3] < stepOrderNum[2] {
				return errors.New("s3 was called before s2")
			}
		}
		return nil
	}

	// Normal, good run.
	w := testTraverseWorkflow(mockRun)
	if err := w.Run(ctx); err != nil {
		t.Errorf("unexpected error: %s", err)
	}
	if err := checkCallOrder(); err != nil {
		t.Errorf("call order error: %s", err)
	}

	callOrder = []int{}
	errs = make([]error, 5)

	// s2 failure.
	w = testTraverseWorkflow(mockRun)
	errs[2] = errors.New("failure")
	want := w.Steps["s2"].wrapRunError(errs[2])
	if err := w.Run(ctx); err.Error() != want.Error() {
		t.Errorf("unexpected error: %s != %s", err, want)
	}
	if err := checkCallOrder(); err != nil {
		t.Errorf("call order error: %s", err)
	}
}

func TestPrint(t *testing.T) {
	data := []byte(`{
"Name": "some-name",
"Project": "some-project",
"Zone": "some-zone",
"GCSPath": "gs://some-bucket/images",
"Vars": {
  "instance_name": "i1",
  "machine_type": {"Value": "n1-standard-1", "Required": true}
},
"Steps": {
  "${instance_name}Delete": {
    "DeleteResources": {
      "Instances": ["${instance_name}"]
    }
  }
}
}`)

	want := `{
  "Name": "some-name",
  "Project": "some-project",
  "Zone": "some-zone",
  "GCSPath": "gs://some-bucket/images",
  "Vars": {
    "instance_name": {
      "Value": "i1",
      "Required": false,
      "Description": ""
    },
    "machine_type": {
      "Value": "n1-standard-1",
      "Required": true,
      "Description": ""
    }
  },
  "Steps": {
    "i1Delete": {
      "Timeout": "10m",
      "DeleteResources": {
        "Instances": [
          "i1"
        ]
      }
    }
  },
  "Dependencies": {}
}
`

	td, err := ioutil.TempDir(os.TempDir(), "")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	defer os.RemoveAll(td)
	tf := filepath.Join(td, "test.wf.json")
	ioutil.WriteFile(tf, data, 0600)

	got, err := NewFromFile(tf)
	if err != nil {
		t.Fatal(err)
	}

	got.ComputeClient, _ = newTestGCEClient()
	got.StorageClient, _ = newTestGCSClient()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	got.Print(context.Background())
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}

	if diff := diff.Diff(buf.String(), want); diff != "" {
		t.Errorf("printed workflow does not match expectation: (-got +want)\n%s", diff)
	}
}

func testValidateErrors(w *Workflow, want string) error {
	if err := w.Validate(context.Background()); err == nil {
		return errors.New("expected error, got nil")
	} else if err.Error() != want {
		return fmt.Errorf("did not get expected error from Validate():\ngot: %q\nwant: %q", err.Error(), want)
	}
	select {
	case <-w.Cancel:
		return nil
	default:
		return errors.New("expected cancel to be closed after error")
	}
}

func TestValidateErrors(t *testing.T) {
	// Error from validateRequiredFields().
	w := testWorkflow()
	w.Name = "1"
	want := "error validating workflow: workflow field 'Name' must start with a letter and only contain letters, numbers, and hyphens"
	if err := testValidateErrors(w, want); err != nil {
		t.Error(err)
	}

	// Error from populate().
	w = testWorkflow()
	w.Steps = map[string]*Step{"s0": {Timeout: "10", testType: &mockStep{}}}
	want = "error populating workflow: time: missing unit in duration 10"
	if err := testValidateErrors(w, want); err != nil {
		t.Error(err)
	}

	// Error from validate().
	w = testWorkflow()
	w.Steps = map[string]*Step{"s0": {testType: &mockStep{}}}
	w.Project = "${var}"
	want = "Unresolved var \"${var}\" found in \"${var}\""
	if err := testValidateErrors(w, want); err != nil {
		t.Error(err)
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	testBucket := "bucket"
	testObject := "object"
	var gotObj string
	var gotBkt string
	nameRgx := regexp.MustCompile(`"name":"([^"].*)"`)
	uploadRgx := regexp.MustCompile(`/b/([^/]+)/o?.*uploadType=multipart.*`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.String()
		m := r.Method
		if match := uploadRgx.FindStringSubmatch(u); m == "POST" && match != nil {
			body, _ := ioutil.ReadAll(r.Body)
			buf.Write(body)
			gotObj = nameRgx.FindStringSubmatch(string(body))[1]
			gotBkt = match[1]
			fmt.Fprintf(w, `{"kind":"storage#object","bucket":"%s","name":"%s"}`, gotBkt, gotObj)
		}

	}))

	gcsClient, err := storage.NewClient(context.Background(), option.WithEndpoint(ts.URL), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	l := gcsLogger{
		client: gcsClient,
		bucket: testBucket,
		object: testObject,
		ctx:    context.Background(),
	}

	tests := []struct {
		test, want string
	}{
		{"test log 1\n", "test log 1\n"},
		{"test log 2\n", "test log 1\ntest log 2\n"},
	}

	for _, tt := range tests {
		l.Write([]byte(tt.test))
		if gotObj != testObject {
			t.Errorf("object does not match, want: %q, got: %q", testObject, gotObj)
		}
		if gotBkt != testBucket {
			t.Errorf("bucket does not match, want: %q, got: %q", testBucket, gotBkt)
		}
		if !strings.Contains(buf.String(), tt.want) {
			t.Errorf("expected text did not get sent to GCS, want: %q, got: %q", tt.want, buf.String())
		}
		if l.buf.String() != tt.want {
			t.Errorf("buffer does mot match expectation, want: %q, got: %q", tt.want, l.buf.String())
		}
	}
}

func TestRunStepTimeout(t *testing.T) {
	w := testWorkflow()
	s, _ := w.NewStep("test")
	s.timeout = 1 * time.Nanosecond
	s.testType = &mockStep{runImpl: func(ctx context.Context, s *Step) error {
		time.Sleep(1 * time.Second)
		return nil
	}}
	want := `step "test" did not stop in specified timeout of 1ns`
	if err := w.runStep(context.Background(), s); err == nil || err.Error() != want {
		t.Errorf("did not get expected error, got: %q, want: %q", err.Error(), want)
	}
}
