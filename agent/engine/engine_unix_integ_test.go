//go:build !windows && integration
// +build !windows,integration

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package engine

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/api"
	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient/sdkclientfactory"
	"github.com/aws/amazon-ecs-agent/agent/statechange"
	"github.com/aws/amazon-ecs-agent/agent/taskresource"
	taskresourcevolume "github.com/aws/amazon-ecs-agent/agent/taskresource/volume"
	"github.com/aws/amazon-ecs-agent/agent/utils"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils/ttime"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/cihub/seelog"
	"github.com/containerd/cgroups/v3"
	"github.com/docker/docker/api/types"
	sdkClient "github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRegistryHost = "127.0.0.1:51670"
	testBusyboxImage = testRegistryHost + "/busybox:latest"
	testVolumeImage  = testRegistryHost + "/amazon/amazon-ecs-volumes-test:latest"
	testFluentdImage = testRegistryHost + "/amazon/fluentd:latest"
)

var (
	endpoint            = utils.DefaultIfBlank(os.Getenv(DockerEndpointEnvVariable), DockerDefaultEndpoint)
	TestGPUInstanceType = []string{"p2", "p3", "g3", "g4dn", "p4d"}
)

// Starting from Docker version 20.10.6, a behavioral change was introduced in docker container port bindings,
// where both IPv4 and IPv6 port bindings will be exposed.
// To mitigate this issue, Agent introduced an environment variable ECS_EXCLUDE_IPV6_PORTBINDING,
// which is true by default in the [PR#3025](https://github.com/aws/amazon-ecs-agent/pull/3025).
// However, the PR does not modify port bindings in the container state change object, it only filters out IPv6 port
// bindings while building the container state change payload. Thus, the invalid IPv6 port bindings still exists
// in ContainerStateChange, and need to be filtered out in this integration test.
//
// The getValidPortBinding function and the ECS_EXCLUDE_IPV6_PORTBINDING environment variable should be removed once
// the IPv6 issue is resolved by Docker and is fully supported by ECS.
func getValidPortBinding(portBindings []apicontainer.PortBinding) []apicontainer.PortBinding {
	validPortBindings := []apicontainer.PortBinding{}
	for _, binding := range portBindings {
		if binding.BindIP == "::" {
			seelog.Debugf("Exclude IPv6 port binding %v", binding)
			continue
		}
		validPortBindings = append(validPortBindings, binding)
	}
	return validPortBindings
}

func createTestHealthCheckTask(arn string) *apitask.Task {
	testTask := &apitask.Task{
		Arn:                 arn,
		Family:              "family",
		Version:             "1",
		DesiredStatusUnsafe: apitaskstatus.TaskRunning,
		Containers:          []*apicontainer.Container{CreateTestContainer()},
	}
	testTask.Containers[0].Image = testBusyboxImage
	testTask.Containers[0].Name = "test-health-check"
	testTask.Containers[0].HealthCheckType = "docker"
	testTask.Containers[0].Command = []string{"sh", "-c", "sleep 300"}
	testTask.Containers[0].DockerConfig = apicontainer.DockerConfig{
		Config: aws.String(alwaysHealthyHealthCheckConfig),
	}
	return testTask
}

// All Namespace Sharing Tests will rely on 3 containers
// container0 will be the container that starts an executable or creates a resource
// container1 and container2 will attempt to see this process/resource
// and quit with exit 0 for success and 1 for failure
func createNamespaceSharingTask(arn, pidMode, ipcMode, testImage string, theCommand []string) *apitask.Task {
	testTask := &apitask.Task{
		Arn:                 arn,
		Family:              "family",
		Version:             "1",
		PIDMode:             pidMode,
		IPCMode:             ipcMode,
		DesiredStatusUnsafe: apitaskstatus.TaskRunning,
		Containers: []*apicontainer.Container{
			&apicontainer.Container{
				Name:                      "container0",
				Image:                     testImage,
				DesiredStatusUnsafe:       apicontainerstatus.ContainerRunning,
				CPU:                       100,
				Memory:                    80,
				TransitionDependenciesMap: make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet),
			},
			&apicontainer.Container{
				Name:                      "container1",
				Image:                     testBusyboxImage,
				Command:                   theCommand,
				DesiredStatusUnsafe:       apicontainerstatus.ContainerRunning,
				CPU:                       100,
				Memory:                    80,
				TransitionDependenciesMap: make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet),
			},
			&apicontainer.Container{
				Name:                      "container2",
				Image:                     testBusyboxImage,
				Command:                   theCommand,
				DesiredStatusUnsafe:       apicontainerstatus.ContainerRunning,
				CPU:                       100,
				Memory:                    80,
				TransitionDependenciesMap: make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet),
			},
		},
	}

	// Setting a container dependency so the executable can be started or resource can be created
	// before read is attempted by other containers
	testTask.Containers[1].BuildContainerDependency(testTask.Containers[0].Name, apicontainerstatus.ContainerRunning, apicontainerstatus.ContainerCreated)
	testTask.Containers[2].BuildContainerDependency(testTask.Containers[0].Name, apicontainerstatus.ContainerRunning, apicontainerstatus.ContainerCreated)
	return testTask
}

func createVolumeTask(t *testing.T, scope, arn, volume string, autoprovision bool) (*apitask.Task, error) {
	tmpDirectory := t.TempDir()
	err := ioutil.WriteFile(filepath.Join(tmpDirectory, "volume-data"), []byte("volume"), 0666)
	if err != nil {
		return nil, err
	}

	testTask := CreateTestTask(arn)

	volumeConfig := &taskresourcevolume.DockerVolumeConfig{
		Scope:  scope,
		Driver: "local",
		DriverOpts: map[string]string{
			"device": tmpDirectory,
			"o":      "bind",
			"type":   "tmpfs",
		},
	}
	if scope == "shared" {
		volumeConfig.Autoprovision = autoprovision
	}

	testTask.Volumes = []apitask.TaskVolume{
		{
			Type:   "docker",
			Name:   volume,
			Volume: volumeConfig,
		},
	}

	testTask.Containers[0].Image = testVolumeImage
	testTask.Containers[0].TransitionDependenciesMap = make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet)
	testTask.Containers[0].MountPoints = []apicontainer.MountPoint{
		{
			SourceVolume:  volume,
			ContainerPath: "/ecs",
		},
	}
	testTask.ResourcesMapUnsafe = make(map[string][]taskresource.TaskResource)
	testTask.Containers[0].Command = []string{"sh", "-c", "if [[ $(cat /ecs/volume-data) != \"volume\" ]]; then cat /ecs/volume-data; exit 1; fi; exit 0"}
	return testTask, nil
}

func TestSharedAutoprovisionVolume(t *testing.T) {
	taskEngine, done, dockerClient, _ := setupWithDefaultConfig(t)
	defer done()
	stateChangeEvents := taskEngine.StateChangeEvents()
	// Set the task clean up duration to speed up the test
	taskEngine.(*DockerTaskEngine).cfg.TaskCleanupWaitDuration = 1 * time.Second

	testTask, err := createVolumeTask(t, "shared", "TestSharedAutoprovisionVolume", "TestSharedAutoprovisionVolume", true)
	require.NoError(t, err, "creating test task failed")

	go taskEngine.AddTask(testTask)

	VerifyTaskIsRunning(stateChangeEvents, testTask)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
	assert.Equal(t, *testTask.Containers[0].GetKnownExitCode(), 0)
	assert.Equal(t, testTask.ResourcesMapUnsafe["dockerVolume"][0].(*taskresourcevolume.VolumeResource).VolumeConfig.DockerVolumeName, "TestSharedAutoprovisionVolume", "task volume name is not the same as specified in task definition")
	// Wait for task to be cleaned up
	testTask.SetSentStatus(apitaskstatus.TaskStopped)
	waitForTaskCleanup(t, taskEngine, testTask.Arn, 5)
	client := taskEngine.(*DockerTaskEngine).client
	response := client.InspectVolume(context.TODO(), "TestSharedAutoprovisionVolume", 1*time.Second)
	assert.NoError(t, response.Error, "expect shared volume not removed")

	cleanVolumes(testTask, dockerClient)
}

func TestSharedDoNotAutoprovisionVolume(t *testing.T) {
	taskEngine, done, dockerClient, _ := setupWithDefaultConfig(t)
	defer done()
	stateChangeEvents := taskEngine.StateChangeEvents()
	client := taskEngine.(*DockerTaskEngine).client
	// Set the task clean up duration to speed up the test
	taskEngine.(*DockerTaskEngine).cfg.TaskCleanupWaitDuration = 1 * time.Second

	testTask, err := createVolumeTask(t, "shared", "TestSharedDoNotAutoprovisionVolume", "TestSharedDoNotAutoprovisionVolume", false)
	require.NoError(t, err, "creating test task failed")

	// creating volume to simulate previously provisioned volume
	volumeConfig := testTask.Volumes[0].Volume.(*taskresourcevolume.DockerVolumeConfig)
	volumeMetadata := client.CreateVolume(context.TODO(), "TestSharedDoNotAutoprovisionVolume",
		volumeConfig.Driver, volumeConfig.DriverOpts, volumeConfig.Labels, 1*time.Minute)
	require.NoError(t, volumeMetadata.Error)

	go taskEngine.AddTask(testTask)

	VerifyTaskIsRunning(stateChangeEvents, testTask)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
	assert.Equal(t, *testTask.Containers[0].GetKnownExitCode(), 0)
	assert.Len(t, testTask.ResourcesMapUnsafe["dockerVolume"], 0, "volume that has been provisioned does not require the agent to create it again")
	// Wait for task to be cleaned up
	testTask.SetSentStatus(apitaskstatus.TaskStopped)
	waitForTaskCleanup(t, taskEngine, testTask.Arn, 5)
	response := client.InspectVolume(context.TODO(), "TestSharedDoNotAutoprovisionVolume", 1*time.Second)
	assert.NoError(t, response.Error, "expect shared volume not removed")

	cleanVolumes(testTask, dockerClient)
}

// TestStartStopUnpulledImage ensures that an unpulled image is successfully
// pulled, run, and stopped via docker.
func TestStartStopUnpulledImage(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	// Ensure this image isn't pulled by deleting it
	removeImage(t, testRegistryImage)

	testTask := CreateTestTask("testStartUnpulled")

	go taskEngine.AddTask(testTask)
	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)
	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
}

// TestStartStopUnpulledImageDigest ensures that an unpulled image with
// specified digest is successfully pulled, run, and stopped via docker.
func TestStartStopUnpulledImageDigest(t *testing.T) {
	imageDigest := "public.ecr.aws/amazonlinux/amazonlinux@sha256:1b6599b4846a765106350130125e2480f6c1cb7791df0ce3e59410362f311259"
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()
	// Ensure this image isn't pulled by deleting it
	removeImage(t, imageDigest)

	testTask := CreateTestTask("testStartUnpulledDigest")
	testTask.Containers[0].Image = imageDigest

	go taskEngine.AddTask(testTask)

	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)
	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
}

// Tests that containers with ordering dependencies are able to reach MANIFEST_PULLED state
// regardless of the dependencies.
func TestManifestPulledDoesNotDependOnContainerOrdering(t *testing.T) {
	imagePullBehaviors := []config.ImagePullBehaviorType{
		config.ImagePullDefaultBehavior, config.ImagePullAlwaysBehavior,
		config.ImagePullPreferCachedBehavior, config.ImagePullOnceBehavior,
	}

	for _, behavior := range imagePullBehaviors {
		t.Run(fmt.Sprintf("%v", behavior), func(t *testing.T) {
			cfg := DefaultTestConfigIntegTest()
			cfg.ImagePullBehavior = behavior
			cfg.DockerStopTimeout = 100 * time.Millisecond
			taskEngine, done, _, _ := SetupIntegTestTaskEngine(cfg, nil, t)
			defer done()

			first := createTestContainerWithImageAndName(testRegistryImage, "first")
			first.Command = GetLongRunningCommand()

			second := createTestContainerWithImageAndName(testRegistryImage, "second")
			second.SetDependsOn([]apicontainer.DependsOn{
				{ContainerName: first.Name, Condition: "COMPLETE"},
			})

			task := &apitask.Task{
				Arn:                 "test-arn",
				Family:              "family",
				Version:             "1",
				DesiredStatusUnsafe: apitaskstatus.TaskRunning,
				Containers:          []*apicontainer.Container{first, second},
			}

			// Start the task and wait for first container to start running
			go taskEngine.AddTask(task)

			// Both containers and the task should reach MANIFEST_PULLED state and emit events for it
			VerifyContainerManifestPulledStateChange(t, taskEngine)
			VerifyContainerManifestPulledStateChange(t, taskEngine)
			VerifyTaskManifestPulledStateChange(t, taskEngine)

			// The first container should start running
			VerifyContainerRunningStateChange(t, taskEngine)

			// The first container should be in RUNNING state
			assert.Equal(t, apicontainerstatus.ContainerRunning, first.GetKnownStatus())
			// The second container should be waiting in MANIFEST_PULLED state
			assert.Equal(t, apicontainerstatus.ContainerManifestPulled, second.GetKnownStatus())

			// Assert that both containers have digest populated
			assert.NotEmpty(t, first.GetImageDigest())
			assert.NotEmpty(t, second.GetImageDigest())

			// Cleanup
			first.SetDesiredStatus(apicontainerstatus.ContainerStopped)
			second.SetDesiredStatus(apicontainerstatus.ContainerStopped)
			VerifyContainerStoppedStateChange(t, taskEngine)
			VerifyContainerStoppedStateChange(t, taskEngine)
			VerifyTaskStoppedStateChange(t, taskEngine)
			taskEngine.(*DockerTaskEngine).removeContainer(task, first)
			taskEngine.(*DockerTaskEngine).removeContainer(task, second)
			removeImage(t, testRegistryImage)
		})
	}
}

// Integration test for pullContainerManifest.
// The test depends on 127.0.0.1:51670/busybox image that is prepared by `make test-registry`
// command.
func TestPullContainerManifestInteg(t *testing.T) {
	allPullBehaviors := []config.ImagePullBehaviorType{
		config.ImagePullDefaultBehavior, config.ImagePullAlwaysBehavior,
		config.ImagePullOnceBehavior, config.ImagePullPreferCachedBehavior,
	}
	tcs := []struct {
		name               string
		image              string
		setConfig          func(c *config.Config)
		imagePullBehaviors []config.ImagePullBehaviorType
		assertError        func(t *testing.T, err error)
	}{
		{
			name:               "digest available in image reference",
			image:              "ubuntu@sha256:c3839dd800b9eb7603340509769c43e146a74c63dca3045a8e7dc8ee07e53966",
			imagePullBehaviors: allPullBehaviors,
		},
		{
			name:               "digest can be resolved from explicit tag",
			image:              localRegistryBusyboxImage,
			imagePullBehaviors: allPullBehaviors,
		},
		{
			name:               "digest can be resolved without an explicit tag",
			image:              localRegistryBusyboxImage,
			imagePullBehaviors: allPullBehaviors,
		},
		{
			name:               "manifest pull can timeout",
			image:              localRegistryBusyboxImage,
			setConfig:          func(c *config.Config) { c.ManifestPullTimeout = 0 },
			imagePullBehaviors: []config.ImagePullBehaviorType{config.ImagePullAlwaysBehavior},

			assertError: func(t *testing.T, err error) {
				assert.ErrorContains(t, err, "Could not transition to MANIFEST_PULLED; timed out")
			},
		},
		{
			name:               "manifest pull can timeout - non-zero timeout",
			image:              localRegistryBusyboxImage,
			setConfig:          func(c *config.Config) { c.ManifestPullTimeout = 100 * time.Microsecond },
			imagePullBehaviors: []config.ImagePullBehaviorType{config.ImagePullAlwaysBehavior},
			assertError: func(t *testing.T, err error) {
				assert.ErrorContains(t, err, "Could not transition to MANIFEST_PULLED; timed out")
			},
		},
	}
	for _, tc := range tcs {
		for _, imagePullBehavior := range tc.imagePullBehaviors {
			t.Run(fmt.Sprintf("%s - %v", tc.name, imagePullBehavior), func(t *testing.T) {
				cfg := DefaultTestConfigIntegTest()
				cfg.ImagePullBehavior = imagePullBehavior

				if tc.setConfig != nil {
					tc.setConfig(cfg)
				}

				taskEngine, done, _, _ := SetupIntegTestTaskEngine(cfg, nil, t)
				defer done()

				container := &apicontainer.Container{Image: tc.image}
				task := &apitask.Task{Containers: []*apicontainer.Container{container}}

				res := taskEngine.(*DockerTaskEngine).pullContainerManifest(task, container)
				if tc.assertError == nil {
					require.NoError(t, res.Error)
					assert.NotEmpty(t, container.GetImageDigest())
				} else {
					tc.assertError(t, res.Error)
				}
			})
		}
	}
}

// Tests pullContainer method pulls container image as expected with and without an image
// digest populated on the container. If an image digest is populated then pullContainer
// uses the digest to prepare a canonical reference for the image to pull the image version
// referenced by the digest.
func TestPullContainerWithAndWithoutDigestInteg(t *testing.T) {
	tcs := []struct {
		name        string
		image       string
		imageDigest string
	}{
		{
			name:        "no tag no digest",
			image:       "public.ecr.aws/docker/library/alpine",
			imageDigest: "",
		},
		{
			name:        "tag but no digest",
			image:       "public.ecr.aws/docker/library/alpine:latest",
			imageDigest: "",
		},
		{
			name:        "no tag with digest",
			image:       "public.ecr.aws/docker/library/alpine",
			imageDigest: "sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b",
		},
		{
			name:        "tag with digest",
			image:       "public.ecr.aws/docker/library/alpine:3.19",
			imageDigest: "sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b",
		},
		{
			name:        "tag and digest with no digest",
			image:       "public.ecr.aws/docker/library/alpine:3.19@sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b",
			imageDigest: "",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare task
			task := &apitask.Task{Containers: []*apicontainer.Container{{Image: tc.image}}}
			container := task.Containers[0]
			container.SetImageDigest(tc.imageDigest)

			// Prepare task engine
			cfg := DefaultTestConfigIntegTest()
			cfg.ImagePullBehavior = config.ImagePullAlwaysBehavior
			taskEngine, done, dockerClient, _ := SetupIntegTestTaskEngine(cfg, nil, t)
			defer done()

			// Remove image from the host if it exists to start from a clean slate
			removeImage(t, container.Image)

			// Pull the image
			pullRes := taskEngine.(*DockerTaskEngine).pullContainer(task, container)
			require.NoError(t, pullRes.Error)

			// Check that the image was pulled
			_, err := dockerClient.InspectImage(container.Image)
			require.NoError(t, err)

			// Cleanup
			removeImage(t, container.Image)
		})
	}
}

// Tests that pullContainer pulls the same image when a digest is used versus when a digest
// is not used.
func TestPullContainerWithAndWithoutDigestConsistency(t *testing.T) {
	image := "public.ecr.aws/docker/library/alpine:3.19.1"
	imageDigest := "sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b"

	// Prepare task
	task := &apitask.Task{Containers: []*apicontainer.Container{{Image: image}}}
	container := task.Containers[0]

	// Prepare task engine
	cfg := DefaultTestConfigIntegTest()
	cfg.ImagePullBehavior = config.ImagePullAlwaysBehavior
	taskEngine, done, dockerClient, _ := SetupIntegTestTaskEngine(cfg, nil, t)
	defer done()

	// Remove image from the host if it exists to start from a clean slate
	removeImage(t, container.Image)

	// Pull the image without digest
	pullRes := taskEngine.(*DockerTaskEngine).pullContainer(task, container)
	require.NoError(t, pullRes.Error)
	inspectWithoutDigest, err := dockerClient.InspectImage(container.Image)
	require.NoError(t, err)
	removeImage(t, container.Image)

	// Pull the image with digest
	container.SetImageDigest(imageDigest)
	pullRes = taskEngine.(*DockerTaskEngine).pullContainer(task, container)
	require.NoError(t, pullRes.Error)
	inspectWithDigest, err := dockerClient.InspectImage(container.Image)
	require.NoError(t, err)
	removeImage(t, container.Image)

	// Image should be the same
	assert.Equal(t, inspectWithDigest.ID, inspectWithoutDigest.ID)
}

// Tests that pullContainer pulls the same image when tag is used versus when a tag
// is not used.
func TestPullContainerWithAndWithoutTagConsistency(t *testing.T) {
	imagewithTag := "public.ecr.aws/docker/library/alpine:3.19.1@sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b"
	imagewithoutTag := "public.ecr.aws/docker/library/alpine@sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b"

	// Prepare task
	task := &apitask.Task{Containers: []*apicontainer.Container{{Image: imagewithTag}}}
	container := task.Containers[0]

	// Prepare task engine
	cfg := DefaultTestConfigIntegTest()
	cfg.ImagePullBehavior = config.ImagePullAlwaysBehavior
	taskEngine, done, dockerClient, _ := SetupIntegTestTaskEngine(cfg, nil, t)
	defer done()

	// Remove image from the host if it exists to start from a clean slate
	removeImage(t, container.Image)

	// Pull the image with tag
	pullRes := taskEngine.(*DockerTaskEngine).pullContainer(task, container)
	require.NoError(t, pullRes.Error)
	inspectWithTag, err := dockerClient.InspectImage(container.Image)
	require.NoError(t, err)
	removeImage(t, container.Image)

	// Prepare task
	task = &apitask.Task{Containers: []*apicontainer.Container{{Image: imagewithoutTag}}}
	container = task.Containers[0]

	// Pull the image without tag
	pullRes = taskEngine.(*DockerTaskEngine).pullContainer(task, container)
	require.NoError(t, pullRes.Error)
	inspectWithoutTag, err := dockerClient.InspectImage(container.Image)
	require.NoError(t, err)
	removeImage(t, container.Image)

	// Image should be the same
	assert.Equal(t, inspectWithTag.ID, inspectWithoutTag.ID)
}

// Tests that a task with invalid image fails as expected.
func TestInvalidImageInteg(t *testing.T) {
	tcs := []struct {
		name          string
		image         string
		expectedError string
	}{
		{
			name:  "repo not found - fails during digest resolution",
			image: "127.0.0.1:51670/invalid-image",
			expectedError: "CannotPullImageManifestError: Error response from daemon: manifest" +
				" unknown: manifest unknown",
		},
		{
			name: "invalid digest provided - fails during pull",
			image: "127.0.0.1:51670/busybox" +
				"@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			expectedError: "CannotPullContainerError: Error response from daemon: manifest for" +
				" 127.0.0.1:51670/busybox" +
				"@sha256:0000000000000000000000000000000000000000000000000000000000000000" +
				" not found: manifest unknown: manifest unknown",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare task engine
			cfg := DefaultTestConfigIntegTest()
			cfg.ImagePullBehavior = config.ImagePullAlwaysBehavior
			taskEngine, done, _, _ := SetupIntegTestTaskEngine(cfg, nil, t)
			defer done()

			// Prepare a task
			container := createTestContainerWithImageAndName(tc.image, "container")
			task := &apitask.Task{
				Arn:                 "test-arn",
				Family:              "family",
				Version:             "1",
				DesiredStatusUnsafe: apitaskstatus.TaskRunning,
				Containers:          []*apicontainer.Container{container},
			}

			// Start the task
			go taskEngine.AddTask(task)

			// The container and the task both should stop
			verifyContainerStoppedStateChangeWithReason(t, taskEngine, tc.expectedError)
			VerifyTaskStoppedStateChange(t, taskEngine)
		})
	}
}

// Tests that a task with an image that has a digest specified works normally.
func TestImageWithDigestInteg(t *testing.T) {
	// Prepare task engine
	cfg := DefaultTestConfigIntegTest()
	cfg.ImagePullBehavior = config.ImagePullAlwaysBehavior
	taskEngine, done, dockerClient, _ := SetupIntegTestTaskEngine(cfg, nil, t)
	defer done()

	// Find image digest
	versionedClient, err := dockerClient.WithVersion(dockerclient.Version_1_35)
	manifest, manifestPullError := versionedClient.PullImageManifest(
		context.Background(), localRegistryBusyboxImage, nil)
	require.NoError(t, manifestPullError)
	imageDigest := manifest.Descriptor.Digest.String()

	// Prepare a task with image digest
	container := createTestContainerWithImageAndName(
		localRegistryBusyboxImage+"@"+imageDigest, "container")
	container.Command = []string{"sh", "-c", "sleep 1"}
	task := &apitask.Task{
		Arn:                 "test-arn",
		Family:              "family",
		Version:             "1",
		DesiredStatusUnsafe: apitaskstatus.TaskRunning,
		Containers:          []*apicontainer.Container{container},
	}

	// Start the task
	go taskEngine.AddTask(task)

	// The task should run. No MANIFEST_PULLED events expected.
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)
	assert.Equal(t, imageDigest, container.GetImageDigest())

	// Cleanup
	container.SetDesiredStatus(apicontainerstatus.ContainerStopped)
	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
	err = taskEngine.(*DockerTaskEngine).removeContainer(task, container)
	require.NoError(t, err, "failed to remove container during cleanup")
	removeImage(t, localRegistryBusyboxImage)
}

// TestPortForward runs a container serving data on the randomly chosen port
// 24751 and verifies that when you do forward the port you can access it and if
// you don't forward the port you can't
func TestPortForward(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testArn := "testPortForwardFail"
	testTask := CreateTestTask(testArn)
	port1 := getUnassignedPort()
	testTask.Containers[0].Command = []string{fmt.Sprintf("-l=%d", port1), "-serve", serverContent}

	// Port not forwarded; verify we can't access it
	go taskEngine.AddTask(testTask)

	err := VerifyTaskIsRunning(stateChangeEvents, testTask)
	require.NoError(t, err)

	time.Sleep(waitForDockerDuration) // wait for Docker
	_, err = net.DialTimeout("tcp", fmt.Sprintf("%s:%d", localhost, port1), dialTimeout)

	require.Error(t, err, "Did not expect to be able to dial %s:%d but didn't get error", localhost, port1)

	// Kill the existing container now to make the test run more quickly.
	containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTask.Arn)
	cid := containerMap[testTask.Containers[0].Name].DockerID
	client, _ := sdkClient.NewClientWithOpts(sdkClient.WithHost(endpoint), sdkClient.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	err = client.ContainerKill(context.TODO(), cid, "SIGKILL")
	require.NoError(t, err, "Could not kill container", err)

	VerifyTaskIsStopped(stateChangeEvents, testTask)

	// Now forward it and make sure that works
	testArn = "testPortForwardWorking"
	testTask = CreateTestTask(testArn)
	port2 := getUnassignedPort()
	testTask.Containers[0].Command = []string{fmt.Sprintf("-l=%d", port2), "-serve", serverContent}
	testTask.Containers[0].Ports = []apicontainer.PortBinding{{ContainerPort: port2, HostPort: port2}}

	taskEngine.AddTask(testTask)

	err = VerifyTaskIsRunning(stateChangeEvents, testTask)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(waitForDockerDuration) // wait for Docker
	conn, err := dialWithRetries("tcp", fmt.Sprintf("%s:%d", localhost, port2), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container " + err.Error())
	}

	var response []byte
	for i := 0; i < 10; i++ {
		response, err = ioutil.ReadAll(conn)
		if err != nil {
			t.Error("Error reading response", err)
		}
		if len(response) > 0 {
			break
		}
		// Retry for a non-blank response. The container in docker 1.7+ sometimes
		// isn't up quickly enough and we get a blank response. It's still unclear
		// to me if this is a docker bug or netkitten bug
		t.Log("Retrying getting response from container; got nothing")
		time.Sleep(100 * time.Millisecond)
	}
	if string(response) != serverContent {
		t.Error("Got response: " + string(response) + " instead of " + serverContent)
	}

	// Stop the existing container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
}

// TestMultiplePortForwards tests that two links containers in the same task can
// both expose ports successfully
func TestMultiplePortForwards(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	// Forward it and make sure that works
	testArn := "testMultiplePortForwards"
	testTask := CreateTestTask(testArn)
	port1 := getUnassignedPort()
	port2 := getUnassignedPort()
	testTask.Containers[0].Command = []string{fmt.Sprintf("-l=%d", port1), "-serve", serverContent + "1"}
	testTask.Containers[0].Ports = []apicontainer.PortBinding{{ContainerPort: port1, HostPort: port1}}
	testTask.Containers[0].Essential = false
	testTask.Containers = append(testTask.Containers, CreateTestContainer())
	testTask.Containers[1].Name = "nc2"
	testTask.Containers[1].Command = []string{fmt.Sprintf("-l=%d", port1), "-serve", serverContent + "2"}
	testTask.Containers[1].Ports = []apicontainer.PortBinding{{ContainerPort: port1, HostPort: port2}}

	go taskEngine.AddTask(testTask)

	err := VerifyTaskIsRunning(stateChangeEvents, testTask)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(waitForDockerDuration) // wait for Docker
	conn, err := dialWithRetries("tcp", fmt.Sprintf("%s:%d", localhost, port1), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container 1 " + err.Error())
	}
	t.Log("Dialed first container")
	response, _ := ioutil.ReadAll(conn)
	if string(response) != serverContent+"1" {
		t.Error("Got response: " + string(response) + " instead of" + serverContent + "1")
	}
	t.Log("Read first container")
	conn, err = dialWithRetries("tcp", fmt.Sprintf("%s:%d", localhost, port2), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container 2 " + err.Error())
	}
	t.Log("Dialed second container")
	response, _ = ioutil.ReadAll(conn)
	if string(response) != serverContent+"2" {
		t.Error("Got response: " + string(response) + " instead of" + serverContent + "2")
	}
	t.Log("Read second container")

	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
}

// TestDynamicPortForward runs a container serving data on a port chosen by the
// docker daemon and verifies that the port is reported in the state-change.
func TestDynamicPortForward(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testArn := "testDynamicPortForward"
	testTask := CreateTestTask(testArn)
	port := getUnassignedPort()
	testTask.Containers[0].Command = []string{fmt.Sprintf("-l=%d", port), "-serve", serverContent}
	// No HostPort = docker should pick
	testTask.Containers[0].Ports = []apicontainer.PortBinding{{ContainerPort: port}}

	go taskEngine.AddTask(testTask)

	event := <-stateChangeEvents
	require.Equal(t, apicontainerstatus.ContainerManifestPulled, event.(api.ContainerStateChange).Status, "Expected container to reach MANIFEST_PULLED state")
	event = <-stateChangeEvents
	require.Equal(t, apitaskstatus.TaskManifestPulled, event.(api.TaskStateChange).Status, "Expected task to reach MANIFEST_PULLED state")
	event = <-stateChangeEvents
	require.Equal(t, event.(api.ContainerStateChange).Status, apicontainerstatus.ContainerRunning, "Expected container to be RUNNING")

	portBindings := event.(api.ContainerStateChange).PortBindings
	// See comments on the getValidPortBinding() function for why ports need to be filtered.
	validPortBindings := getValidPortBinding(portBindings)

	VerifyTaskRunningStateChange(t, taskEngine)

	if len(validPortBindings) != 1 {
		t.Error("PortBindings was not set; should have been len 1", portBindings)
	}
	var bindingForcontainerPortOne uint16
	for _, binding := range validPortBindings {
		if port == binding.ContainerPort {
			bindingForcontainerPortOne = binding.HostPort
		}
	}
	if bindingForcontainerPortOne == 0 {
		t.Errorf("Could not find the port mapping for %d!", port)
	}

	time.Sleep(waitForDockerDuration) // wait for Docker
	conn, err := dialWithRetries("tcp", localhost+":"+strconv.Itoa(int(bindingForcontainerPortOne)), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container " + err.Error())
	}

	response, _ := ioutil.ReadAll(conn)
	if string(response) != serverContent {
		t.Error("Got response: " + string(response) + " instead of " + serverContent)
	}

	// Kill the existing container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
}

func TestMultipleDynamicPortForward(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testArn := "testDynamicPortForward2"
	testTask := CreateTestTask(testArn)
	port := getUnassignedPort()
	testTask.Containers[0].Command = []string{fmt.Sprintf("-l=%d", port), "-serve", serverContent, `-loop`}
	// No HostPort or 0 hostport; docker should pick two ports for us
	testTask.Containers[0].Ports = []apicontainer.PortBinding{{ContainerPort: port}, {ContainerPort: port, HostPort: 0}}

	go taskEngine.AddTask(testTask)

	event := <-stateChangeEvents
	require.Equal(t, apicontainerstatus.ContainerManifestPulled, event.(api.ContainerStateChange).Status, "Expected container to reach MANIFEST_PULLED state")
	event = <-stateChangeEvents
	require.Equal(t, apitaskstatus.TaskManifestPulled, event.(api.TaskStateChange).Status, "Expected task to reach MANIFEST_PULLED state")
	event = <-stateChangeEvents
	require.Equal(t, event.(api.ContainerStateChange).Status, apicontainerstatus.ContainerRunning, "Expected container to be RUNNING")

	portBindings := event.(api.ContainerStateChange).PortBindings
	// See comments on the getValidPortBinding() function for why ports need to be filtered.
	validPortBindings := getValidPortBinding(portBindings)

	VerifyTaskRunningStateChange(t, taskEngine)

	if len(validPortBindings) != 2 {
		t.Error("Could not bind to two ports from one container port", portBindings)
	}
	var bindingForcontainerPortOne_1 uint16
	var bindingForcontainerPortOne_2 uint16
	for _, binding := range validPortBindings {
		if port == binding.ContainerPort {
			if bindingForcontainerPortOne_1 == 0 {
				bindingForcontainerPortOne_1 = binding.HostPort
			} else {
				bindingForcontainerPortOne_2 = binding.HostPort
			}
		}
	}
	if bindingForcontainerPortOne_1 == 0 {
		t.Errorf("Could not find the port mapping for %d!", port)
	}
	if bindingForcontainerPortOne_2 == 0 {
		t.Errorf("Could not find the port mapping for %d!", port)
	}

	time.Sleep(waitForDockerDuration) // wait for Docker
	conn, err := dialWithRetries("tcp", localhost+":"+strconv.Itoa(int(bindingForcontainerPortOne_1)), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container " + err.Error())
	}

	response, _ := ioutil.ReadAll(conn)
	if string(response) != serverContent {
		t.Error("Got response: " + string(response) + " instead of " + serverContent)
	}

	conn, err = dialWithRetries("tcp", localhost+":"+strconv.Itoa(int(bindingForcontainerPortOne_2)), 10, dialTimeout)
	if err != nil {
		t.Fatal("Error dialing simple container " + err.Error())
	}

	response, _ = ioutil.ReadAll(conn)
	if string(response) != serverContent {
		t.Error("Got response: " + string(response) + " instead of " + serverContent)
	}

	// Kill the existing container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
}

// TestLinking ensures that container linking does allow networking to go
// through to a linked container.  this test specifically starts a server that
// prints "hello linker" and then links a container that proxies that data to
// a publicly exposed port, where the tests read it
func TestLinking(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	testArn := "TestLinking"
	testTask := CreateTestTask(testArn)
	testTask.Containers = append(testTask.Containers, CreateTestContainer())
	testTask.Containers[0].Command = []string{"-l=80", "-serve", "hello linker"}
	testTask.Containers[0].Name = "linkee"
	port := getUnassignedPort()
	testTask.Containers[1].Command = []string{fmt.Sprintf("-l=%d", port), "linkee_alias:80"}
	testTask.Containers[1].Links = []string{"linkee:linkee_alias"}
	testTask.Containers[1].Ports = []apicontainer.PortBinding{{ContainerPort: port, HostPort: port}}

	stateChangeEvents := taskEngine.StateChangeEvents()

	go taskEngine.AddTask(testTask)

	err := VerifyTaskIsRunning(stateChangeEvents, testTask)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(waitForDockerDuration)

	var response []byte
	for i := 0; i < 10; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", localhost, port), dialTimeout)
		if err != nil {
			t.Log("Error dialing simple container" + err.Error())
		}
		response, err = ioutil.ReadAll(conn)
		if err != nil {
			t.Error("Error reading response", err)
		}
		if len(response) > 0 {
			break
		}
		// Retry for a non-blank response. The container in docker 1.7+ sometimes
		// isn't up quickly enough and we get a blank response. It's still unclear
		// to me if this is a docker bug or netkitten bug
		t.Log("Retrying getting response from container; got nothing")
		time.Sleep(500 * time.Millisecond)
	}
	if string(response) != "hello linker" {
		t.Error("Got response: " + string(response) + " instead of 'hello linker'")
	}

	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)

	VerifyTaskIsStopped(stateChangeEvents, testTask)
}

func TestVolumesFromRO(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testTask := CreateTestTask("testVolumeROContainer")
	testTask.Containers[0].Image = testVolumeImage
	for i := 0; i < 3; i++ {
		cont := CreateTestContainer()
		cont.Name = "test" + strconv.Itoa(i)
		cont.Image = testVolumeImage
		cont.Essential = i > 0
		testTask.Containers = append(testTask.Containers, cont)
	}
	testTask.Containers[1].VolumesFrom = []apicontainer.VolumeFrom{{SourceContainer: testTask.Containers[0].Name, ReadOnly: true}}
	testTask.Containers[1].Command = []string{"touch /data/readonly-fs || exit 42"}
	// make all the three containers non-essential to make sure all of the
	// container can be transitioned to running even one of them finished first
	testTask.Containers[1].Essential = false
	testTask.Containers[2].VolumesFrom = []apicontainer.VolumeFrom{{SourceContainer: testTask.Containers[0].Name}}
	testTask.Containers[2].Command = []string{"touch /data/notreadonly-fs-1 || exit 42"}
	testTask.Containers[2].Essential = false
	testTask.Containers[3].VolumesFrom = []apicontainer.VolumeFrom{{SourceContainer: testTask.Containers[0].Name, ReadOnly: false}}
	testTask.Containers[3].Command = []string{"touch /data/notreadonly-fs-2 || exit 42"}
	testTask.Containers[3].Essential = false

	go taskEngine.AddTask(testTask)

	VerifyTaskIsRunning(stateChangeEvents, testTask)
	taskEngine.(*DockerTaskEngine).stopContainer(testTask, testTask.Containers[0])

	VerifyTaskIsStopped(stateChangeEvents, testTask)

	if testTask.Containers[1].GetKnownExitCode() == nil || *testTask.Containers[1].GetKnownExitCode() != 42 {
		t.Error("Didn't exit due to failure to touch ro fs as expected: ", testTask.Containers[1].GetKnownExitCode())
	}
	if testTask.Containers[2].GetKnownExitCode() == nil || *testTask.Containers[2].GetKnownExitCode() != 0 {
		t.Error("Couldn't touch with default of rw")
	}
	if testTask.Containers[3].GetKnownExitCode() == nil || *testTask.Containers[3].GetKnownExitCode() != 0 {
		t.Error("Couldn't touch with explicit rw")
	}
}

func createTestHostVolumeMountTask(tmpPath string) *apitask.Task {
	testTask := CreateTestTask("testHostVolumeMount")
	testTask.Volumes = []apitask.TaskVolume{{Name: "test-tmp", Volume: &taskresourcevolume.FSHostVolume{FSSourcePath: tmpPath}}}
	testTask.Containers[0].Image = testVolumeImage
	testTask.Containers[0].MountPoints = []apicontainer.MountPoint{{ContainerPath: "/host/tmp", SourceVolume: "test-tmp"}}
	testTask.Containers[0].Command = []string{`echo -n "hi" > /host/tmp/hello-from-container; if [[ "$(cat /host/tmp/test-file)" != "test-data" ]]; then exit 4; fi; exit 42`}
	return testTask
}

// This integ test is meant to validate the docker assumptions related to
// https://github.com/aws/amazon-ecs-agent/issues/261
// Namely, this test verifies that Docker does emit a 'die' event after an OOM
// event if the init dies.
// Note: Your kernel must support swap limits in order for this test to run.
// See https://github.com/docker/docker/pull/4251 about enabling swap limit
// support, or set MY_KERNEL_DOES_NOT_SUPPORT_SWAP_LIMIT to non-empty to skip
// this test.
func TestInitOOMEvent(t *testing.T) {
	if os.Getenv("MY_KERNEL_DOES_NOT_SUPPORT_SWAP_LIMIT") != "" {
		t.Skip("Skipped because MY_KERNEL_DOES_NOT_SUPPORT_SWAP_LIMIT")
	}
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testTask := CreateTestTask("oomtest")
	testTask.Containers[0].Memory = 20
	testTask.Containers[0].Image = testBusyboxImage
	testTask.Containers[0].Command = []string{"sh", "-c", `x="a"; while true; do x=$x$x$x; done`}
	// should cause sh to get oomkilled as pid 1

	go taskEngine.AddTask(testTask)

	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	event := <-stateChangeEvents
	require.Equal(t, event.(api.ContainerStateChange).Status, apicontainerstatus.ContainerStopped, "Expected container to be STOPPED")

	// hold on to the container stopped event, will need to check exit code
	contEvent := event.(api.ContainerStateChange)

	VerifyTaskStoppedStateChange(t, taskEngine)

	if contEvent.ExitCode == nil {
		t.Error("Expected exitcode to be set")
	} else if *contEvent.ExitCode != 137 {
		t.Errorf("Expected exitcode to be 137, not %v", *contEvent.ExitCode)
	}

	dockerVersion, err := taskEngine.Version()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dockerVersion, " 1.9.") {
		// Skip the final check for some versions of docker
		t.Logf("Docker version is 1.9.x (%s); not checking OOM reason", dockerVersion)
		return
	}
	if !strings.HasPrefix(contEvent.Reason, dockerapi.OutOfMemoryError{}.ErrorName()) {
		t.Errorf("Expected reason to have OOM error, was: %v", contEvent.Reason)
	}
}

// This integ test exercises the Docker "kill" facility, which exists to send
// signals to PID 1 inside a container.  Starting with Docker 1.7, a `kill`
// event was emitted by the Docker daemon on any `kill` invocation.
// Signals used in this test:
// SIGTERM - sent by Docker "stop" prior to SIGKILL (9)
// SIGUSR1 - used for the test as an arbitrary signal
func TestSignalEvent(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	testArn := "signaltest"
	testTask := CreateTestTask(testArn)
	testTask.Containers[0].Image = testBusyboxImage
	testTask.Containers[0].Command = []string{
		"sh",
		"-c",
		fmt.Sprintf(`trap "exit 42" %d; trap "echo signal!" %d; while true; do sleep 1; done`, int(syscall.SIGTERM), int(syscall.SIGUSR1)),
	}

	go taskEngine.AddTask(testTask)

	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	// Signal the container now
	containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTask.Arn)
	cid := containerMap[testTask.Containers[0].Name].DockerID
	client, _ := sdkClient.NewClientWithOpts(sdkClient.WithHost(endpoint), sdkClient.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	err := client.ContainerKill(context.TODO(), cid, "SIGUSR1")
	require.NoError(t, err, "Could not signal container", err)

	// Verify the container has not stopped
	time.Sleep(2 * time.Second)
check_events:
	for {
		select {
		case event := <-stateChangeEvents:
			if event.GetEventType() == statechange.ContainerEvent {
				contEvent := event.(api.ContainerStateChange)
				if contEvent.TaskArn != testTask.Arn {
					continue
				}
				t.Fatalf("Expected no events; got " + contEvent.Status.String())
			}
		default:
			break check_events
		}
	}

	// Stop the container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)

	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)

	if testTask.Containers[0].GetKnownExitCode() == nil || *testTask.Containers[0].GetKnownExitCode() != 42 {
		t.Error("Wrong exit code; file probably wasn't present")
	}
}

// TestDockerStopTimeout tests the container was killed after ECS_CONTAINER_STOP_TIMEOUT
func TestDockerStopTimeout(t *testing.T) {
	os.Setenv("ECS_CONTAINER_STOP_TIMEOUT", testDockerStopTimeout.String())
	defer os.Unsetenv("ECS_CONTAINER_STOP_TIMEOUT")
	cfg := DefaultTestConfigIntegTest()

	taskEngine, _, _, _ := SetupIntegTestTaskEngine(cfg, nil, t)

	dockerTaskEngine := taskEngine.(*DockerTaskEngine)

	if dockerTaskEngine.cfg.DockerStopTimeout != testDockerStopTimeout {
		t.Errorf("Expect the docker stop timeout read from environment variable when ECS_CONTAINER_STOP_TIMEOUT is set, %v", dockerTaskEngine.cfg.DockerStopTimeout)
	}
	testTask := CreateTestTask("TestDockerStopTimeout")
	testTask.Containers[0].Command = []string{"sh", "-c", "trap 'echo hello' SIGTERM; while true; do echo `date +%T`; sleep 1s; done;"}
	testTask.Containers[0].Image = testBusyboxImage
	testTask.Containers[0].Name = "test-docker-timeout"

	go dockerTaskEngine.AddTask(testTask)

	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	startTime := ttime.Now()
	dockerTaskEngine.stopContainer(testTask, testTask.Containers[0])

	VerifyContainerStoppedStateChange(t, taskEngine)

	if ttime.Since(startTime) < testDockerStopTimeout {
		t.Errorf("Container stopped before the timeout: %v", ttime.Since(startTime))
	}
	if ttime.Since(startTime) > testDockerStopTimeout+1*time.Second {
		t.Errorf("Container should have stopped eariler, but stopped after %v", ttime.Since(startTime))
	}
}

func TestStartStopWithSecurityOptionNoNewPrivileges(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	testArn := "testSecurityOptionNoNewPrivileges"
	testTask := CreateTestTask(testArn)
	testTask.Containers[0].DockerConfig = apicontainer.DockerConfig{HostConfig: aws.String(`{"SecurityOpt":["no-new-privileges"]}`)}

	go taskEngine.AddTask(testTask)

	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	// Kill the existing container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)

	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
}

func TestTaskLevelVolume(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()
	stateChangeEvents := taskEngine.StateChangeEvents()

	testTask, err := createVolumeTask(t, "task", "TestTaskLevelVolume", "TestTaskLevelVolume", true)
	require.NoError(t, err, "creating test task failed")

	go taskEngine.AddTask(testTask)

	VerifyTaskIsRunning(stateChangeEvents, testTask)
	VerifyTaskIsStopped(stateChangeEvents, testTask)
	require.Equal(t, *testTask.Containers[0].GetKnownExitCode(), 0)
	require.NotEqual(t, testTask.ResourcesMapUnsafe["dockerVolume"][0].(*taskresourcevolume.VolumeResource).VolumeConfig.Source(), "TestTaskLevelVolume", "task volume name is the same as specified in task definition")

	client := taskEngine.(*DockerTaskEngine).client
	client.RemoveVolume(context.TODO(), "TestTaskLevelVolume", 5*time.Second)
}

func TestSwapConfigurationTask(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	client, err := sdkClient.NewClientWithOpts(sdkClient.WithHost(endpoint), sdkClient.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	require.NoError(t, err, "Creating go docker client failed")

	testArn := "TestSwapMemory"
	testTask := CreateTestTask(testArn)
	testTask.Containers[0].DockerConfig = apicontainer.DockerConfig{HostConfig: aws.String(`{"MemorySwap":314572800, "MemorySwappiness":90}`)}

	go taskEngine.AddTask(testTask)
	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTask.Arn)
	cid := containerMap[testTask.Containers[0].Name].DockerID
	state, _ := client.ContainerInspect(ctx, cid)
	require.EqualValues(t, 314572800, state.HostConfig.MemorySwap)
	// skip testing memory swappiness for cgroupv2, since this control has been removed in cgroupv2
	if cgroups.Mode() != cgroups.Unified {
		require.EqualValues(t, 90, *state.HostConfig.MemorySwappiness)
	}

	// Kill the existing container now
	taskUpdate := CreateTestTask(testArn)
	taskUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(taskUpdate)

	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
}

func TestPerContainerStopTimeout(t *testing.T) {
	// set the global stop timemout, but this should be ignored since the per container value is set
	globalStopContainerTimeout := 1000 * time.Second
	os.Setenv("ECS_CONTAINER_STOP_TIMEOUT", globalStopContainerTimeout.String())
	defer os.Unsetenv("ECS_CONTAINER_STOP_TIMEOUT")
	cfg := DefaultTestConfigIntegTest()

	taskEngine, _, _, _ := SetupIntegTestTaskEngine(cfg, nil, t)

	dockerTaskEngine := taskEngine.(*DockerTaskEngine)

	if dockerTaskEngine.cfg.DockerStopTimeout != globalStopContainerTimeout {
		t.Errorf("Expect ECS_CONTAINER_STOP_TIMEOUT to be set to , %v", dockerTaskEngine.cfg.DockerStopTimeout)
	}

	testTask := CreateTestTask("TestDockerStopTimeout")
	testTask.Containers[0].Command = []string{"sh", "-c", "trap 'echo hello' SIGTERM; while true; do echo `date +%T`; sleep 1s; done;"}
	testTask.Containers[0].Image = testBusyboxImage
	testTask.Containers[0].Name = "test-docker-timeout"
	testTask.Containers[0].StopTimeout = uint(testDockerStopTimeout.Seconds())

	go dockerTaskEngine.AddTask(testTask)

	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	startTime := ttime.Now()
	dockerTaskEngine.stopContainer(testTask, testTask.Containers[0])

	VerifyContainerStoppedStateChange(t, taskEngine)

	if ttime.Since(startTime) < testDockerStopTimeout {
		t.Errorf("Container stopped before the timeout: %v", ttime.Since(startTime))
	}
	if ttime.Since(startTime) > testDockerStopTimeout+1*time.Second {
		t.Errorf("Container should have stopped eariler, but stopped after %v", ttime.Since(startTime))
	}
}

func TestMemoryOverCommit(t *testing.T) {
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()
	memoryReservation := 50

	client, err := sdkClient.NewClientWithOpts(sdkClient.WithHost(endpoint), sdkClient.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	require.NoError(t, err, "Creating go docker client failed")

	testArn := "TestMemoryOverCommit"
	testTask := CreateTestTask(testArn)

	testTask.Containers[0].DockerConfig = apicontainer.DockerConfig{HostConfig: aws.String(`{
	"MemoryReservation": 52428800 }`)}

	go taskEngine.AddTask(testTask)
	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTask.Arn)
	cid := containerMap[testTask.Containers[0].Name].DockerID
	state, _ := client.ContainerInspect(ctx, cid)

	require.EqualValues(t, memoryReservation*1024*1024, state.HostConfig.MemoryReservation)

	// Kill the existing container now
	testUpdate := CreateTestTask(testArn)
	testUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(testUpdate)

	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)
}

// TestNetworkModeHost tests the container network can be configured
// as bridge mode in task definition
func TestNetworkModeHost(t *testing.T) {
	testNetworkMode(t, "bridge")
}

// TestNetworkModeBridge tests the container network can be configured
// as host mode in task definition
func TestNetworkModeBridge(t *testing.T) {
	testNetworkMode(t, "host")
}

func TestFluentdTag(t *testing.T) {
	// Skipping the test for arm as they do not have official support for Arm images
	if runtime.GOARCH == "arm64" {
		t.Skip("Skipping test, unsupported image for arm64")
	}

	logdir := os.TempDir()
	logdir = path.Join(logdir, "ftslog")
	defer os.RemoveAll(logdir)

	os.Setenv("ECS_AVAILABLE_LOGGING_DRIVERS", `["fluentd"]`)
	defer os.Unsetenv("ECS_AVAILABLE_LOGGING_DRIVERS")

	taskEngine, _, _, _ := setupWithDefaultConfig(t)

	client, err := sdkClient.NewClientWithOpts(sdkClient.WithHost(endpoint),
		sdkClient.WithVersion(sdkclientfactory.GetDefaultVersion().String()))
	require.NoError(t, err, "Creating go docker client failed")

	// start Fluentd driver task
	testTaskFleuntdDriver := CreateTestTask("testFleuntdDriver")
	testTaskFleuntdDriver.Volumes = []apitask.TaskVolume{{Name: "logs", Volume: &taskresourcevolume.FSHostVolume{FSSourcePath: "/tmp"}}}
	testTaskFleuntdDriver.Containers[0].Image = testFluentdImage
	testTaskFleuntdDriver.Containers[0].MountPoints = []apicontainer.MountPoint{{ContainerPath: "/fluentd/log",
		SourceVolume: "logs"}}
	testTaskFleuntdDriver.Containers[0].Ports = []apicontainer.PortBinding{{ContainerPort: 24224, HostPort: 24224}}
	go taskEngine.AddTask(testTaskFleuntdDriver)
	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	// Sleep before starting the test task so that fluentd driver is setup
	time.Sleep(30 * time.Second)

	// start fluentd log task
	testTaskFluentdLogTag := CreateTestTask("testFleuntdTag")
	testTaskFluentdLogTag.Containers[0].Command = []string{"/bin/echo", "hello, this is fluentd integration test"}
	testTaskFluentdLogTag.Containers[0].Image = testBusyboxImage
	testTaskFluentdLogTag.Containers[0].DockerConfig = apicontainer.DockerConfig{
		HostConfig: aws.String(`{"LogConfig": {
		"Type": "fluentd",
		"Config": {
			"fluentd-address":"0.0.0.0:24224",
			"tag":"ecs.{{.Name}}.{{.FullID}}"
		}
	}}`)}

	go taskEngine.AddTask(testTaskFluentdLogTag)
	VerifyContainerManifestPulledStateChange(t, taskEngine)
	VerifyTaskManifestPulledStateChange(t, taskEngine)
	VerifyContainerRunningStateChange(t, taskEngine)
	VerifyTaskRunningStateChange(t, taskEngine)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTaskFluentdLogTag.Arn)
	cid := containerMap[testTaskFluentdLogTag.Containers[0].Name].DockerID
	state, _ := client.ContainerInspect(ctx, cid)

	// Kill the fluentd driver task
	testUpdate := CreateTestTask("testFleuntdDriver")
	testUpdate.SetDesiredStatus(apitaskstatus.TaskStopped)
	go taskEngine.AddTask(testUpdate)
	VerifyContainerStoppedStateChange(t, taskEngine)
	VerifyTaskStoppedStateChange(t, taskEngine)

	logTag := fmt.Sprintf("ecs.%v.%v", strings.Replace(state.Name,
		"/", "", 1), cid)

	// Verify the log file existed and also the content contains the expected format
	err = utils.SearchStrInDir(logdir, "ecsfts", "hello, this is fluentd integration test")
	require.NoError(t, err, "failed to find the content in the fluent log file")

	err = utils.SearchStrInDir(logdir, "ecsfts", logTag)
	require.NoError(t, err, "failed to find the log tag specified in the task definition")
}

func TestDockerExecAPI(t *testing.T) {
	testTimeout := 1 * time.Minute
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	taskArn := "testDockerExec"
	testTask := CreateTestTask(taskArn)

	A := createTestContainerWithImageAndName(baseImageForOS, "A")

	A.EntryPoint = &entryPointForOS
	A.Command = []string{"sleep 30"}
	A.Essential = true

	testTask.Containers = []*apicontainer.Container{
		A,
	}
	execConfig := types.ExecConfig{
		User:   "0",
		Detach: true,
		Cmd:    []string{"ls"},
	}
	go taskEngine.AddTask(testTask)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	finished := make(chan interface{})
	go func() {
		// Both containers should start
		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, testTask)

		containerMap, _ := taskEngine.(*DockerTaskEngine).state.ContainerMapByArn(testTask.Arn)
		dockerID := containerMap[testTask.Containers[0].Name].DockerID

		//Create Exec object on the host
		execContainerOut, err := taskEngine.(*DockerTaskEngine).client.CreateContainerExec(ctx, dockerID, execConfig, dockerclient.ContainerExecCreateTimeout)
		require.NoError(t, err)
		require.NotNil(t, execContainerOut)

		//Start the above Exec process on the host
		err1 := taskEngine.(*DockerTaskEngine).client.StartContainerExec(ctx, execContainerOut.ID, types.ExecStartCheck{Detach: true, Tty: false},
			dockerclient.ContainerExecStartTimeout)
		require.NoError(t, err1)

		//Inspect the above Exec process on the host to check if the exit code is 0 which indicates successful run of the command
		execContInspectOut, err := taskEngine.(*DockerTaskEngine).client.InspectContainerExec(ctx, execContainerOut.ID, dockerclient.ContainerExecInspectTimeout)
		require.NoError(t, err)
		require.Equal(t, dockerID, execContInspectOut.ContainerID)
		require.Equal(t, 0, execContInspectOut.ExitCode)

		// Task should stop
		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, testTask)
		close(finished)
	}()

	waitFinished(t, finished, testTimeout)
}

// This integ test checks for task queuing behavior in waitingTaskQueue which is dependent on hostResourceManager.
// First two tasks totally consume the available memory resource on the host. So the third task queued up needs to wait
// until resources gets freed up (i.e. any running tasks stops and frees enough resources) before it can start progressing.
func TestHostResourceManagerTrickleQueue(t *testing.T) {
	testTimeout := 1 * time.Minute
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	tasks := []*apitask.Task{}
	for i := 0; i < 3; i++ {
		taskArn := fmt.Sprintf("taskArn-%d", i)
		testTask := CreateTestTask(taskArn)

		// create container
		A := createTestContainerWithImageAndName(baseImageForOS, "A")
		A.EntryPoint = &entryPointForOS
		A.Command = []string{"sleep 10"}
		A.Essential = true
		testTask.Containers = []*apicontainer.Container{
			A,
		}

		// task memory so that only 2 such tasks can run - 1024 total memory available on instance by getTestHostResources()
		testTask.Memory = int64(512)

		tasks = append(tasks, testTask)
	}

	// goroutine to trickle tasks to enforce queueing order
	go func() {
		taskEngine.AddTask(tasks[0])
		time.Sleep(2 * time.Second)
		taskEngine.AddTask(tasks[1])
		time.Sleep(2 * time.Second)
		taskEngine.AddTask(tasks[2])
	}()

	finished := make(chan interface{})

	// goroutine to verify task running order
	go func() {
		// Tasks go RUNNING in order
		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[0])

		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[1])

		// First task should stop before 3rd task goes RUNNING
		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[0])

		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[2])

		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[1])

		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[2])
		close(finished)
	}()

	// goroutine to verify task accounting
	// After ~4s, 3rd task should be queued up and will not be dequeued until ~10s, i.e. until 1st task stops and gets dequeued
	go func() {
		time.Sleep(6 * time.Second)
		task, err := taskEngine.(*DockerTaskEngine).topTask()
		assert.NoError(t, err, "one task should be queued up after 6s")
		assert.Equal(t, task.Arn, tasks[2].Arn, "wrong task at top of queue")

		time.Sleep(6 * time.Second)
		_, err = taskEngine.(*DockerTaskEngine).topTask()
		assert.Error(t, err, "no task should be queued up after 12s")
	}()
	waitFinished(t, finished, testTimeout)
}

// This test verifies if a task which is STOPPING does not block other new tasks
// from starting if resources for them are available
func TestHostResourceManagerResourceUtilization(t *testing.T) {
	testTimeout := 1 * time.Minute
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	tasks := []*apitask.Task{}
	for i := 0; i < 2; i++ {
		taskArn := fmt.Sprintf("IntegTaskArn-%d", i)
		testTask := CreateTestTask(taskArn)

		// create container
		A := createTestContainerWithImageAndName(baseImageForOS, fmt.Sprintf("A-%d", i))
		A.EntryPoint = &entryPointForOS
		A.Command = []string{"trap shortsleep SIGTERM; shortsleep() { sleep 6; exit 1; }; sleep 10"}
		A.Essential = true
		A.StopTimeout = uint(6)
		testTask.Containers = []*apicontainer.Container{
			A,
		}

		tasks = append(tasks, testTask)
	}

	// Stop task payload from ACS for 1st task
	stopTask := CreateTestTask("IntegTaskArn-0")
	stopTask.DesiredStatusUnsafe = apitaskstatus.TaskStopped
	stopTask.Containers = []*apicontainer.Container{}

	go func() {
		taskEngine.AddTask(tasks[0])
		time.Sleep(2 * time.Second)

		// single managedTask which should have started
		assert.Equal(t, 1, len(taskEngine.(*DockerTaskEngine).managedTasks), "exactly one task should be running")

		// stopTask
		taskEngine.AddTask(stopTask)
		time.Sleep(2 * time.Second)

		taskEngine.AddTask(tasks[1])
	}()

	finished := make(chan interface{})

	// goroutine to verify task running order
	go func() {
		// Tasks go RUNNING in order, 2nd task doesn't wait for 1st task
		// to transition to STOPPED as resources are available
		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[0])

		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[1])

		// At this time, task[0] stopTask is received, and SIGTERM sent to task
		// but the task[0] is still RUNNING due to trap handler
		assert.Equal(t, apitaskstatus.TaskRunning, tasks[0].GetKnownStatus(), "task 0 known status should be RUNNING")
		assert.Equal(t, apitaskstatus.TaskStopped, tasks[0].GetDesiredStatus(), "task 0 status should be STOPPED")

		// task[0] stops after SIGTERM trap handler finishes
		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[0])

		// task[1] stops after normal execution
		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[1])

		close(finished)
	}()

	waitFinished(t, finished, testTimeout)
}

// This task verifies resources are properly released for all tasks for the case where
// stopTask is received from ACS for a task which is queued up in waitingTasksQueue
func TestHostResourceManagerStopTaskNotBlockWaitingTasks(t *testing.T) {
	testTimeout := 1 * time.Minute
	taskEngine, done, _, _ := setupWithDefaultConfig(t)
	defer done()

	stateChangeEvents := taskEngine.StateChangeEvents()

	tasks := []*apitask.Task{}
	stopTasks := []*apitask.Task{}
	for i := 0; i < 2; i++ {
		taskArn := fmt.Sprintf("IntegTaskArn-%d", i)
		testTask := CreateTestTask(taskArn)
		testTask.Memory = int64(768)

		// create container
		A := createTestContainerWithImageAndName(baseImageForOS, fmt.Sprintf("A-%d", i))
		A.EntryPoint = &entryPointForOS
		A.Command = []string{"trap shortsleep SIGTERM; shortsleep() { sleep 6; exit 1; }; sleep 10"}
		A.Essential = true
		A.StopTimeout = uint(6)
		testTask.Containers = []*apicontainer.Container{
			A,
		}

		tasks = append(tasks, testTask)

		// Stop task payloads from ACS for the tasks
		stopTask := CreateTestTask(fmt.Sprintf("IntegTaskArn-%d", i))
		stopTask.DesiredStatusUnsafe = apitaskstatus.TaskStopped
		stopTask.Containers = []*apicontainer.Container{}
		stopTasks = append(stopTasks, stopTask)
	}

	// goroutine to schedule tasks
	go func() {
		taskEngine.AddTask(tasks[0])
		time.Sleep(2 * time.Second)

		// single managedTask which should have started
		assert.Equal(t, 1, len(taskEngine.(*DockerTaskEngine).managedTasks), "exactly one task should be running")

		// stopTask[0] - stop running task[0], this task will go to STOPPING due to trap handler defined and STOPPED after 6s
		taskEngine.AddTask(stopTasks[0])

		time.Sleep(2 * time.Second)

		// this task (task[1]) goes in waitingTasksQueue because not enough memory available
		taskEngine.AddTask(tasks[1])

		time.Sleep(2 * time.Second)

		// stopTask[1] - stop waiting task - task[1]
		taskEngine.AddTask(stopTasks[1])
	}()

	finished := make(chan interface{})

	// goroutine to verify task running order and verify assertions
	go func() {
		// First task goes to MANIFEST_PULLED
		VerifyContainerManifestPulledStateChange(t, taskEngine)
		VerifyTaskManifestPulledStateChange(t, taskEngine)

		// 1st task goes to RUNNING
		VerifyContainerRunningStateChange(t, taskEngine)
		VerifyTaskIsRunning(stateChangeEvents, tasks[0])

		time.Sleep(2500 * time.Millisecond)

		// At this time, task[0] stopTask is received, and SIGTERM sent to task
		// but the task[0] is still RUNNING due to trap handler
		assert.Equal(t, apitaskstatus.TaskRunning, tasks[0].GetKnownStatus(), "task 0 known status should be RUNNING")
		assert.Equal(t, apitaskstatus.TaskStopped, tasks[0].GetDesiredStatus(), "task 0 status should be STOPPED")

		time.Sleep(2 * time.Second)

		// task[1] stops while in waitingTasksQueue while task[0] is in progress
		// This is because it is still waiting to progress, has no containers created
		// and does not need to wait for stopTimeout, can immediately STSC out
		VerifyTaskIsStopped(stateChangeEvents, tasks[1])

		// task[0] stops
		VerifyContainerStoppedStateChange(t, taskEngine)
		VerifyTaskIsStopped(stateChangeEvents, tasks[0])

		// Verify resources are properly released in host resource manager
		assert.False(t, taskEngine.(*DockerTaskEngine).hostResourceManager.checkTaskConsumed(tasks[0].Arn), "task 0 resources not released")
		assert.False(t, taskEngine.(*DockerTaskEngine).hostResourceManager.checkTaskConsumed(tasks[1].Arn), "task 1 resources not released")

		close(finished)
	}()

	waitFinished(t, finished, testTimeout)
}

// Test Host Resource Manager does not account Fargate tasks when started
func TestHostResourceManagerLaunchTypeBehavior(t *testing.T) {
	testCases := []struct {
		Name       string
		LaunchType string
	}{
		{
			Name:       "TestHostResourceManagerFargateLaunchTypeBehavior",
			LaunchType: "FARGATE",
		},
		{
			Name:       "TestHostResourceManagerEC2LaunchTypeBehavior",
			LaunchType: "EC2",
		},
		{
			Name:       "TestHostResourceManagerExternalLaunchTypeBehavior",
			LaunchType: "EXTERNAL",
		},
		{
			Name:       "TestHostResourceManagerRandomLaunchTypeBehavior",
			LaunchType: "RaNdOmStrInG",
		},
		{
			Name:       "TestHostResourceManagerEmptyLaunchTypeBehavior",
			LaunchType: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			testTimeout := 1 * time.Minute
			taskEngine, done, _, _ := setupWithDefaultConfig(t)
			defer done()

			stateChangeEvents := taskEngine.StateChangeEvents()

			taskArn := "IntegTaskArn"
			testTask := CreateTestTask(taskArn)
			testTask.Memory = int64(768)
			testTask.LaunchType = tc.LaunchType

			// create container
			taskContainer := createTestContainerWithImageAndName(baseImageForOS, "SleepWithTrap")
			taskContainer.EntryPoint = &entryPointForOS
			taskContainer.Command = []string{"trap shortsleep SIGTERM; shortsleep() { sleep 6; exit 1; }; sleep 10"}
			taskContainer.Essential = true
			taskContainer.StopTimeout = uint(6)
			testTask.Containers = []*apicontainer.Container{
				taskContainer,
			}

			// Stop task payloads from ACS for the tasks
			stopTask := CreateTestTask("IntegTaskArn")
			stopTask.DesiredStatusUnsafe = apitaskstatus.TaskStopped
			stopTask.Containers = []*apicontainer.Container{}

			// goroutine to schedule tasks
			go func() {
				taskEngine.AddTask(testTask)
				time.Sleep(2 * time.Second)

				// single managedTask which should have started
				assert.Equal(t, 1, len(taskEngine.(*DockerTaskEngine).managedTasks), "exactly one task should be running")

				// stopTask - stop running task, this task will go to STOPPING due to trap handler defined and STOPPED after 6s
				taskEngine.AddTask(stopTask)
			}()

			finished := make(chan interface{})

			// goroutine to verify task running order and verify assertions
			go func() {
				// Task goes to RUNNING
				VerifyContainerManifestPulledStateChange(t, taskEngine)
				VerifyTaskManifestPulledStateChange(t, taskEngine)
				VerifyContainerRunningStateChange(t, taskEngine)
				VerifyTaskIsRunning(stateChangeEvents, testTask)

				time.Sleep(2500 * time.Millisecond)

				// At this time, stopTask is received, and SIGTERM sent to task
				// but the task is still RUNNING due to trap handler
				assert.Equal(t, apitaskstatus.TaskRunning, testTask.GetKnownStatus(), "task known status should be RUNNING")
				assert.Equal(t, apitaskstatus.TaskStopped, testTask.GetDesiredStatus(), "task desired status should be STOPPED")
				// Verify resources are properly consumed in host resource manager, and not consumed for Fargate
				if tc.LaunchType == "FARGATE" {
					assert.False(t, taskEngine.(*DockerTaskEngine).hostResourceManager.checkTaskConsumed(testTask.Arn), "fargate task resources should not be consumed")
				} else {
					assert.True(t, taskEngine.(*DockerTaskEngine).hostResourceManager.checkTaskConsumed(testTask.Arn), "non fargate task resources should be consumed")
				}
				time.Sleep(2 * time.Second)
				close(finished)
			}()

			waitFinished(t, finished, testTimeout)
		})
	}
}
