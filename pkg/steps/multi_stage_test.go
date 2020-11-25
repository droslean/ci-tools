package steps

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/sets"
	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowdapi "k8s.io/test-infra/prow/pod-utils/downwardapi"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/testhelper"
)

func TestRequires(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config api.ReleaseBuildConfiguration
		steps  api.MultiStageTestConfigurationLiteral
		req    []api.StepLink
	}{{
		name: "step has a cluster profile and requires a release image, should not have ReleaseImagesLink",
		steps: api.MultiStageTestConfigurationLiteral{
			ClusterProfile: api.ClusterProfileAWS,
			Test:           []api.LiteralTestStep{{From: "from-release"}},
		},
		req: []api.StepLink{
			api.ReleasePayloadImageLink(api.LatestReleaseName),
			api.ImagesReadyLink(),
		},
	}, {
		name: "step needs release images, should have ReleaseImagesLink",
		steps: api.MultiStageTestConfigurationLiteral{
			Test: []api.LiteralTestStep{{From: "from-release"}},
		},
		req: []api.StepLink{
			api.ReleaseImagesLink(api.LatestReleaseName),
		},
	}, {
		name: "step needs images, should have InternalImageLink",
		config: api.ReleaseBuildConfiguration{
			Images: []api.ProjectDirectoryImageBuildStepConfiguration{
				{To: "from-images"},
			},
		},
		steps: api.MultiStageTestConfigurationLiteral{
			Test: []api.LiteralTestStep{{From: "from-images"}},
		},
		req: []api.StepLink{api.InternalImageLink("from-images")},
	}, {
		name: "step needs pipeline image, should have InternalImageLink",
		steps: api.MultiStageTestConfigurationLiteral{
			Test: []api.LiteralTestStep{{From: "src"}},
		},
		req: []api.StepLink{
			api.InternalImageLink(
				api.PipelineImageStreamTagReferenceSource),
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			step := MultiStageTestStep(api.TestStepConfiguration{
				MultiStageTestConfigurationLiteral: &tc.steps,
			}, &tc.config, api.NewDeferredParameters(nil), nil, "", nil, nil)
			ret := step.Requires()
			if len(ret) == len(tc.req) {
				matches := true
				for i := range ret {
					if !ret[i].SatisfiedBy(tc.req[i]) {
						matches = false
						break
					}
				}
				if matches {
					return
				}
			}
			t.Errorf("incorrect requirements: %s", cmp.Diff(ret, tc.req, api.Comparer()))
		})
	}
}

func TestGeneratePods(t *testing.T) {
	env := []coreapi.EnvVar{
		{Name: "RELEASE_IMAGE_INITIAL", Value: "release:initial"},
		{Name: "RELEASE_IMAGE_LATEST", Value: "release:latest"},
		{Name: "LEASED_RESOURCE", Value: "uuid"},
	}
	hostnameEnvValue := "test.hostname"

	testCases := []struct {
		id      string
		config  api.ReleaseBuildConfiguration
		jobSpec api.JobSpec
	}{{

		id: "happy case",
		config: api.ReleaseBuildConfiguration{
			Tests: []api.TestStepConfiguration{
				{
					As: "test",
					MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
						ClusterProfile: api.ClusterProfileAWS,
						Test: []api.LiteralTestStep{
							{
								As: "step0", From: "src", Commands: "command0",
							},
							{
								As:          "step1",
								From:        "image1",
								Commands:    "command1",
								ArtifactDir: "/artifact/dir",
								Environment: []api.StepParameter{
									{
										Name:    "TEST_HOSTNAME",
										Default: &hostnameEnvValue,
									},
								},
								HostAliases: []coreapi.HostAlias{
									{
										IP:        "10.0.0.1",
										Hostnames: []string{"api.${TEST_HOSTNAME}.com", "test2"},
									},
									{
										IP:        "10.0.0.2",
										Hostnames: []string{"api.$TEST_HOSTNAME.com", "test4"},
									},
								},
							},
							{
								As: "step2", From: "stable-initial:installer", Commands: "command2",
							},
						},
					},
				},
			},
		},
		jobSpec: api.JobSpec{
			JobSpec: prowdapi.JobSpec{
				Job:       "job",
				BuildID:   "build id",
				ProwJobID: "prow job id",
				Refs: &prowapi.Refs{
					Org:     "org",
					Repo:    "repo",
					BaseRef: "base ref",
					BaseSHA: "base sha",
				},
				Type: "postsubmit",
			},
		},
	},
	}

	for _, tc := range testCases {
		tc.jobSpec.SetNamespace("namespace")
		step := newMultiStageTestStep(tc.config.Tests[0], &tc.config, nil, nil, "artifact_dir", &tc.jobSpec, nil)

		ret, err := step.generatePods(tc.config.Tests[0].MultiStageTestConfigurationLiteral.Test, env, false)
		if err != nil {
			t.Fatalf("test %s failed with error: %v", tc.id, err)
		}
		testhelper.CompareWithFixture(t, ret)
	}
}

func TestGeneratePodsEnvironment(t *testing.T) {
	value := "test"
	defValue := "default"
	for _, tc := range []struct {
		name     string
		env      api.TestEnvironment
		test     api.LiteralTestStep
		expected *string
	}{{
		name: "test environment is propagated to the step",
		env:  api.TestEnvironment{"TEST": "test"},
		test: api.LiteralTestStep{
			Environment: []api.StepParameter{{Name: "TEST"}},
		},
		expected: &value,
	}, {
		name: "test environment is not propagated to the step",
		env:  api.TestEnvironment{"TEST": "test"},
		test: api.LiteralTestStep{
			Environment: []api.StepParameter{{Name: "NOT_TEST"}},
		},
	}, {
		name: "default value is overwritten",
		env:  api.TestEnvironment{"TEST": "test"},
		test: api.LiteralTestStep{
			Environment: []api.StepParameter{{
				Name:    "TEST",
				Default: &defValue,
			}},
		},
		expected: &value,
	}, {
		name: "default value is applied",
		test: api.LiteralTestStep{
			Environment: []api.StepParameter{{
				Name:    "TEST",
				Default: &defValue,
			}},
		},
		expected: &defValue,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			jobSpec := api.JobSpec{
				JobSpec: prowdapi.JobSpec{
					Job:       "job",
					BuildID:   "build_id",
					ProwJobID: "prow_job_id",
					Type:      prowapi.PeriodicJob,
				},
			}
			jobSpec.SetNamespace("ns")
			test := []api.LiteralTestStep{tc.test}
			step := MultiStageTestStep(api.TestStepConfiguration{
				MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
					Test:        test,
					Environment: tc.env,
				},
			}, &api.ReleaseBuildConfiguration{}, nil, nil, "", &jobSpec, nil)
			pods, err := step.(*multiStageTestStep).generatePods(test, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			var env *string
			for i, v := range pods[0].Spec.Containers[0].Env {
				if v.Name == "TEST" {
					env = &pods[0].Spec.Containers[0].Env[i].Value
				}
			}
			if !reflect.DeepEqual(env, tc.expected) {
				t.Errorf("incorrect environment:\n%s", diff.ObjectReflectDiff(env, tc.expected))
			}
		})
	}
}

func TestGeneratePodReadonly(t *testing.T) {
	config := api.ReleaseBuildConfiguration{
		Tests: []api.TestStepConfiguration{{
			As: "test",
			MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
				Test: []api.LiteralTestStep{{
					As:                "step0",
					From:              "src",
					Commands:          "command0",
					ReadonlySharedDir: true,
				}},
			},
		}},
	}
	jobSpec := api.JobSpec{
		JobSpec: prowdapi.JobSpec{
			Job:       "job",
			BuildID:   "build id",
			ProwJobID: "prow job id",
			Refs: &prowapi.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "base ref",
				BaseSHA: "base sha",
			},
			Type: "postsubmit",
		},
	}
	jobSpec.SetNamespace("namespace")
	step := newMultiStageTestStep(config.Tests[0], &config, nil, nil, "artifact_dir", &jobSpec, nil)
	ret, err := step.generatePods(config.Tests[0].MultiStageTestConfigurationLiteral.Test, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	testhelper.CompareWithFixture(t, ret)
}

type fakePodExecutor struct {
	ctrlruntimeclient.Client
	failures    sets.String
	createdPods []*coreapi.Pod
}

func (f *fakePodExecutor) Create(ctx context.Context, o ctrlruntimeclient.Object, opts ...ctrlruntimeclient.CreateOption) error {
	if pod, ok := o.(*coreapi.Pod); ok {
		if pod.Namespace == "" {
			return errors.New("pod had no namespace set")
		}
		f.createdPods = append(f.createdPods, pod.DeepCopy())
		pod.Status.Phase = coreapi.PodPending
	}
	return f.Client.Create(ctx, o, opts...)
}

func (f *fakePodExecutor) Get(ctx context.Context, n ctrlruntimeclient.ObjectKey, o ctrlruntimeclient.Object) error {
	if err := f.Client.Get(ctx, n, o); err != nil {
		return err
	}
	if pod, ok := o.(*coreapi.Pod); ok {
		fail := f.failures.Has(n.Name)
		if fail {
			pod.Status.Phase = coreapi.PodFailed
		} else {
			pod.Status.Phase = coreapi.PodSucceeded
		}
		for _, container := range pod.Spec.Containers {
			terminated := &coreapi.ContainerStateTerminated{}
			if fail {
				terminated.ExitCode = 1
			}
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, coreapi.ContainerStatus{
				Name:  container.Name,
				State: coreapi.ContainerState{Terminated: terminated}})
		}
	}

	return nil
}

func TestRun(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name     string
		failures sets.String
		expected []string
	}{{
		name: "no step fails, no error",
		expected: []string{
			"test-pre0", "test-pre1",
			"test-test0", "test-test1",
			"test-post0",
		},
	}, {
		name:     "failure in a pre step, test should not run, post should",
		failures: sets.NewString("test-pre0"),
		expected: []string{
			"test-pre0",
			"test-post0", "test-post1",
		},
	}, {
		name:     "failure in a test step, post should run",
		failures: sets.NewString("test-test0"),
		expected: []string{
			"test-pre0", "test-pre1",
			"test-test0",
			"test-post0", "test-post1",
		},
	}, {
		name:     "failure in a post step, other post steps should still run",
		failures: sets.NewString("test-post0"),
		expected: []string{
			"test-pre0", "test-pre1",
			"test-test0", "test-test1",
			"test-post0",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			name := "test"
			crclient := &fakePodExecutor{Client: fakectrlruntimeclient.NewFakeClient(), failures: tc.failures}
			jobSpec := api.JobSpec{
				JobSpec: prowdapi.JobSpec{
					Job:       "job",
					BuildID:   "build_id",
					ProwJobID: "prow_job_id",
					Type:      prowapi.PeriodicJob,
				},
			}
			jobSpec.SetNamespace("ns")
			step := MultiStageTestStep(api.TestStepConfiguration{
				As: name,
				MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
					Pre:                []api.LiteralTestStep{{As: "pre0"}, {As: "pre1"}},
					Test:               []api.LiteralTestStep{{As: "test0"}, {As: "test1"}},
					Post:               []api.LiteralTestStep{{As: "post0"}, {As: "post1", OptionalOnSuccess: &yes}},
					AllowSkipOnSuccess: &yes,
				},
			}, &api.ReleaseBuildConfiguration{}, nil, &fakePodClient{fakePodExecutor: crclient}, "", &jobSpec, nil)
			if err := step.Run(context.Background()); (err != nil) != (tc.failures != nil) {
				t.Errorf("expected error: %t, got error: %v", (tc.failures != nil), err)
			}
			secrets := &coreapi.SecretList{}
			if err := crclient.List(context.TODO(), secrets, ctrlruntimeclient.InNamespace(jobSpec.Namespace())); err != nil {
				t.Fatal(err)
			}
			if l := secrets.Items; len(l) != 1 || l[0].ObjectMeta.Name != name {
				t.Errorf("unexpected secrets: %#v", l)
			}
			var names []string
			for _, pod := range crclient.createdPods {
				if pod.Namespace != jobSpec.Namespace() {
					t.Errorf("pod %s didn't have namespace %s set, had %q instead", pod.Name, jobSpec.Namespace(), pod.Namespace)
				}
				names = append(names, pod.Name)
			}
			if diff := cmp.Diff(names, tc.expected); diff != "" {
				t.Errorf("did not execute correct pods: %s, actual: %v, expected: %v", diff, names, tc.expected)
			}
		})
	}
}

func TestArtifacts(t *testing.T) {
	timeSecond = time.Nanosecond
	defer func() { timeSecond = time.Second }()

	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	ns := "namespace"
	jobSpec := api.JobSpec{
		JobSpec: prowdapi.JobSpec{
			Job:       "job",
			BuildID:   "build_id",
			ProwJobID: "prow_job_id",
			Type:      prowapi.PeriodicJob,
		},
	}
	jobSpec.SetNamespace(ns)
	testName := "test"
	step := MultiStageTestStep(api.TestStepConfiguration{
		As: testName,
		MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
			Test: []api.LiteralTestStep{
				{As: "test0", ArtifactDir: "/path/to/artifacts"},
				{As: "test1", ArtifactDir: "/path/to/artifacts"},
			},
		},
	}, &api.ReleaseBuildConfiguration{}, nil, &fakePodClient{fakePodExecutor: &fakePodExecutor{Client: fakectrlruntimeclient.NewFakeClient()}}, tmp, &jobSpec, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := step.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, x := range []string{"test0", "test1"} {
		if _, err := os.Stat(filepath.Join(tmp, testName, x)); err != nil {
			t.Fatalf("error verifying output directory %q exists: %v", x, err)
		}
	}
}

func TestJUnit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures sets.String
		expected []string
	}{{
		name: "no step fails",
		expected: []string{
			"Run multi-stage test test - test-pre0 container test",
			"Run multi-stage test test - test-pre1 container test",
			"Run multi-stage test test - test-test0 container test",
			"Run multi-stage test test - test-test1 container test",
			"Run multi-stage test test - test-post0 container test",
			"Run multi-stage test test - test-post1 container test",
		},
	}, {
		name:     "failure in a pre step",
		failures: sets.NewString("test-pre0"),
		expected: []string{
			"Run multi-stage test test - test-pre0 container test",
			"Run multi-stage test test - test-post0 container test",
			"Run multi-stage test test - test-post1 container test",
		},
	}, {
		name:     "failure in a test step",
		failures: sets.NewString("test-test0"),
		expected: []string{
			"Run multi-stage test test - test-pre0 container test",
			"Run multi-stage test test - test-pre1 container test",
			"Run multi-stage test test - test-test0 container test",
			"Run multi-stage test test - test-post0 container test",
			"Run multi-stage test test - test-post1 container test",
		},
	}, {
		name:     "failure in a post step",
		failures: sets.NewString("test-post1"),
		expected: []string{
			"Run multi-stage test test - test-pre0 container test",
			"Run multi-stage test test - test-pre1 container test",
			"Run multi-stage test test - test-test0 container test",
			"Run multi-stage test test - test-test1 container test",
			"Run multi-stage test test - test-post0 container test",
			"Run multi-stage test test - test-post1 container test",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakePodExecutor{Client: fakectrlruntimeclient.NewFakeClient(), failures: tc.failures}
			jobSpec := api.JobSpec{
				JobSpec: prowdapi.JobSpec{
					Job:       "job",
					BuildID:   "build_id",
					ProwJobID: "prow_job_id",
					Type:      prowapi.PeriodicJob,
				},
			}
			jobSpec.SetNamespace("test-namespace")
			step := MultiStageTestStep(api.TestStepConfiguration{
				As: "test",
				MultiStageTestConfigurationLiteral: &api.MultiStageTestConfigurationLiteral{
					Pre:  []api.LiteralTestStep{{As: "pre0"}, {As: "pre1"}},
					Test: []api.LiteralTestStep{{As: "test0"}, {As: "test1"}},
					Post: []api.LiteralTestStep{{As: "post0"}, {As: "post1"}},
				},
			}, &api.ReleaseBuildConfiguration{}, nil, &fakePodClient{fakePodExecutor: client}, "/dev/null", &jobSpec, nil)
			if err := step.Run(context.Background()); tc.failures == nil && err != nil {
				t.Error(err)
				return
			}
			var names []string
			for _, t := range step.(subtestReporter).SubTests() {
				names = append(names, t.Name)
			}
			if !reflect.DeepEqual(names, tc.expected) {
				t.Error(diff.ObjectReflectDiff(names, tc.expected))
			}
		})
	}
}

func TestAddCredentials(t *testing.T) {
	var testCases = []struct {
		name        string
		credentials []api.CredentialReference
		pod         coreapi.Pod
		expected    coreapi.Pod
	}{
		{
			name:        "none to add",
			credentials: []api.CredentialReference{},
			pod:         coreapi.Pod{},
			expected:    coreapi.Pod{},
		},
		{
			name:        "one to add",
			credentials: []api.CredentialReference{{Namespace: "ns", Name: "name", MountPath: "/tmp"}},
			pod: coreapi.Pod{Spec: coreapi.PodSpec{
				Containers: []coreapi.Container{{VolumeMounts: []coreapi.VolumeMount{}}},
				Volumes:    []coreapi.Volume{},
			}},
			expected: coreapi.Pod{Spec: coreapi.PodSpec{
				Containers: []coreapi.Container{{VolumeMounts: []coreapi.VolumeMount{{Name: "ns-name", MountPath: "/tmp"}}}},
				Volumes:    []coreapi.Volume{{Name: "ns-name", VolumeSource: coreapi.VolumeSource{Secret: &coreapi.SecretVolumeSource{SecretName: "ns-name"}}}},
			}},
		},
		{
			name: "many to add and disambiguate",
			credentials: []api.CredentialReference{
				{Namespace: "ns", Name: "name", MountPath: "/tmp"},
				{Namespace: "other", Name: "name", MountPath: "/tamp"},
			},
			pod: coreapi.Pod{Spec: coreapi.PodSpec{
				Containers: []coreapi.Container{{VolumeMounts: []coreapi.VolumeMount{}}},
				Volumes:    []coreapi.Volume{},
			}},
			expected: coreapi.Pod{Spec: coreapi.PodSpec{
				Containers: []coreapi.Container{{VolumeMounts: []coreapi.VolumeMount{
					{Name: "ns-name", MountPath: "/tmp"},
					{Name: "other-name", MountPath: "/tamp"},
				}}},
				Volumes: []coreapi.Volume{
					{Name: "ns-name", VolumeSource: coreapi.VolumeSource{Secret: &coreapi.SecretVolumeSource{SecretName: "ns-name"}}},
					{Name: "other-name", VolumeSource: coreapi.VolumeSource{Secret: &coreapi.SecretVolumeSource{SecretName: "other-name"}}},
				},
			}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			addCredentials(testCase.credentials, &testCase.pod)
			if !equality.Semantic.DeepEqual(testCase.pod, testCase.expected) {
				t.Errorf("%s: got incorrect Pod: %s", testCase.name, cmp.Diff(testCase.pod, testCase.expected))
			}
		})
	}
}

func TestResolveHostAliases(t *testing.T) {
	testCases := []struct {
		id          string
		envVars     []coreapi.EnvVar
		hostAliases []coreapi.HostAlias
		expected    []coreapi.HostAlias
	}{
		{
			id:          "happy case, no env vars",
			hostAliases: []coreapi.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"hostname1"}}},
			expected:    []coreapi.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"hostname1"}}},
		},
		{
			id: "happy case, with env vars ${var} style",
			envVars: []coreapi.EnvVar{
				{
					Name:  "TEST_IP",
					Value: "10.0.0.100",
				},
				{
					Name:  "TEST_HOSTNAME",
					Value: "test.hostname",
				},
			},
			hostAliases: []coreapi.HostAlias{{IP: "${TEST_IP}", Hostnames: []string{"api.${TEST_HOSTNAME}.com"}}},
			expected:    []coreapi.HostAlias{{IP: "10.0.0.100", Hostnames: []string{"api.test.hostname.com"}}},
		},
		{
			id: "happy case, with env vars $var style",
			envVars: []coreapi.EnvVar{
				{
					Name:  "TEST_IP",
					Value: "10.0.0.100",
				},
				{
					Name:  "TEST_HOSTNAME",
					Value: "test.hostname",
				},
			},
			hostAliases: []coreapi.HostAlias{{IP: "$TEST_IP", Hostnames: []string{"api.$TEST_HOSTNAME.com"}}},
			expected:    []coreapi.HostAlias{{IP: "10.0.0.100", Hostnames: []string{"api.test.hostname.com"}}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.id, func(t *testing.T) {
			resolveHostAliases(tc.hostAliases, tc.envVars)
			if !reflect.DeepEqual(tc.hostAliases, tc.expected) {
				t.Fatal(cmp.Diff(tc.hostAliases, tc.expected))
			}

		})
	}

}
