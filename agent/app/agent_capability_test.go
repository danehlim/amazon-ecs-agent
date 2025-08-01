//go:build unit
// +build unit

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

package app

import (
	"context"
	"errors"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	app_mocks "github.com/aws/amazon-ecs-agent/agent/app/mocks"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient"
	mock_dockerapi "github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi/mocks"
	mock_ecscni "github.com/aws/amazon-ecs-agent/agent/ecscni/mocks"
	dm "github.com/aws/amazon-ecs-agent/agent/engine/daemonmanager"
	mock_daemonmanager "github.com/aws/amazon-ecs-agent/agent/engine/daemonmanager/mock"
	mock_serviceconnect "github.com/aws/amazon-ecs-agent/agent/engine/serviceconnect/mock"
	mock_loader "github.com/aws/amazon-ecs-agent/agent/utils/loader/mocks"
	mock_mobypkgwrapper "github.com/aws/amazon-ecs-agent/agent/utils/mobypkgwrapper/mocks"
	"github.com/aws/amazon-ecs-agent/ecs-agent/ipcompatibility"
	md "github.com/aws/amazon-ecs-agent/ecs-agent/manageddaemon"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	mockPathExists(false)
}

func mockPathExists(shouldExist bool) {
	pathExists = func(path string, shouldBeDirectory bool) (bool, error) {
		return shouldExist, nil
	}
}

func TestCapabilities(t *testing.T) {
	mockPathExists(true)
	defer mockPathExists(false)
	getSubDirectories = func(path string) ([]string, error) {
		// appendExecCapabilities() requires at least 1 version to exist
		return []string{"3.0.236.0"}, nil
	}
	defer func() {
		getSubDirectories = defaultGetSubDirectories
	}()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	cniClient := mock_ecscni.NewMockCNIClient(ctrl)
	mockCredentialsProvider := app_mocks.NewMockCredentialsProvider(ctrl)
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	conf := &config.Config{
		AvailableLoggingDrivers: []dockerclient.LoggingDriver{
			dockerclient.JSONFileDriver,
			dockerclient.SyslogDriver,
			dockerclient.JournaldDriver,
			dockerclient.GelfDriver,
			dockerclient.FluentdDriver,
		},
		PrivilegedDisabled:         config.BooleanDefaultFalse{Value: config.ExplicitlyDisabled},
		SELinuxCapable:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AppArmorCapable:            config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskENIEnabled:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AWSVPCBlockInstanceMetdata: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskCleanupWaitDuration:    config.DefaultConfig(ipcompatibility.NewIPv4OnlyCompatibility()).TaskCleanupWaitDuration,
	}

	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes().Return([]string{"ecs.capability.service-connect-v1"}, nil)

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}

	md.ImportAll = func() ([]*md.ManagedDaemon, error) {
		return []*md.ManagedDaemon{}, nil
	}

	// Scan() and ListPluginsWithFilters() are tested with
	// AnyTimes() because they are not called in windows.
	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
			dockerclient.Version_1_17,
			dockerclient.Version_1_18,
			dockerclient.Version_1_19,
		}),
		// CNI plugins are platform dependent.
		// Therefore, for any version query for any plugin return an appropriate version
		cniClient.EXPECT().Version(gomock.Any()).Return("v1", nil),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)

	// TODO add capabilityEBSTaskAttach
	expectedNameOnlyCapabilities := []string{
		capabilityPrefix + "privileged-container",
		capabilityPrefix + "docker-remote-api.1.17",
		capabilityPrefix + "docker-remote-api.1.18",
		capabilityPrefix + "logging-driver.json-file",
		capabilityPrefix + "logging-driver.syslog",
		capabilityPrefix + "logging-driver.journald",
		capabilityPrefix + "selinux",
		capabilityPrefix + "apparmor",
		attributePrefix + "docker-plugin.local",
		attributePrefix + taskENIAttributeSuffix,
		attributePrefix + capabilityPrivateRegistryAuthASM,
		attributePrefix + capabilitySecretEnvSSM,
		attributePrefix + capabilitySecretLogDriverSSM,
		attributePrefix + capabilityECREndpoint,
		attributePrefix + capabilitySecretEnvASM,
		attributePrefix + capabilitySecretLogDriverASM,
		attributePrefix + capabilityContainerOrdering,
		attributePrefix + capabilityFullTaskSync,
		attributePrefix + capabilityEnvFilesS3,
		attributePrefix + taskENIBlockInstanceMetadataAttributeSuffix,
		attributePrefix + capabilityExec,
		attributePrefix + capabilityServiceConnect,
		attributePrefix + capabilityContainerPortRange,
		attributePrefix + capabilityContainerRestartPolicy,
	}

	var expectedCapabilities []types.Attribute
	for _, name := range expectedNameOnlyCapabilities {
		expectedCapabilities = append(expectedCapabilities,
			types.Attribute{Name: aws.String(name)})
	}
	expectedCapabilities = append(expectedCapabilities,
		[]types.Attribute{
			{
				Name:  aws.String(attributePrefix + cniPluginVersionSuffix),
				Value: aws.String("v1"),
			},
		}...)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		cniClient:             cniClient,
		pauseLoader:           mockPauseLoader,
		credentialsCache:      aws.NewCredentialsCache(mockCredentialsProvider),
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}
	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	for _, expected := range expectedCapabilities {
		assert.Contains(t, capabilities, types.Attribute{
			Name:  expected.Name,
			Value: expected.Value,
		})
	}
}

// Test external capability by checking that when external config is set, capabilities not supported on external capacity
// aren't added, external specific capabilities are added, and capabilities common for both external and non-external are added.
func TestCapabilitiesExternal(t *testing.T) {
	cfg := getCapabilitiesTestConfig()
	capsNonExternal := getCapabilitiesWithConfig(cfg, t)
	cfg.External = config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled}
	capsExternal := getCapabilitiesWithConfig(cfg, t)

	for _, cap := range externalUnsupportedCapabilities {
		assert.NotContains(t, capsExternal, types.Attribute{
			Name: aws.String(cap),
		})
	}
	for _, cap := range externalSpecificCapabilities {
		assert.Contains(t, capsExternal, types.Attribute{
			Name: aws.String(cap),
		})
	}
	commonCaps := removeAttributesByNames(capsNonExternal, externalUnsupportedCapabilities)
	for _, cap := range commonCaps {
		assert.Contains(t, capsExternal, cap)
	}
}

func getCapabilitiesTestConfig() *config.Config {
	return &config.Config{
		AvailableLoggingDrivers: []dockerclient.LoggingDriver{
			dockerclient.JSONFileDriver,
			dockerclient.SyslogDriver,
			dockerclient.JournaldDriver,
			dockerclient.GelfDriver,
			dockerclient.FluentdDriver,
		},
		PrivilegedDisabled:         config.BooleanDefaultFalse{Value: config.ExplicitlyDisabled},
		SELinuxCapable:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AppArmorCapable:            config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskENIEnabled:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AWSVPCBlockInstanceMetdata: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskCleanupWaitDuration:    config.DefaultConfig(ipcompatibility.NewIPv4OnlyCompatibility()).TaskCleanupWaitDuration,
	}
}

func getCapabilitiesWithConfig(cfg *config.Config, t *testing.T) []types.Attribute {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	mockCredentialsProvider := app_mocks.NewMockCredentialsProvider(ctrl)
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockCNIClient := mock_ecscni.NewMockCNIClient(ctrl)

	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	// CNI plugins are platform dependent. Therefore return version for any plugin query.
	mockCNIClient.EXPECT().Version(gomock.Any()).Return("v1", nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
			dockerclient.Version_1_17,
			dockerclient.Version_1_18,
		}),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   cfg,
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		cniClient:             mockCNIClient,
		credentialsCache:      aws.NewCredentialsCache(mockCredentialsProvider),
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}
	capabilities, err := agent.capabilities()
	require.NoError(t, err)
	return capabilities
}

func TestCapabilitiesECR(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	conf := &config.Config{}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_19,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}
	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["com.amazonaws.ecs.capability.ecr-auth"]
	assert.True(t, ok, "Could not find ECR capability when expected; got capabilities %v", capabilities)

	_, ok = capMap["ecs.capability.execution-role-ecr-pull"]
	assert.True(t, ok, "Could not find ECR execution pull capability when expected; got capabilities %v", capabilities)
}

func TestCapabilitiesTaskIAMRoleForSupportedDockerVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	conf := &config.Config{
		TaskIAMRoleEnabled: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_19,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	ok := capMap["com.amazonaws.ecs.capability.task-iam-role"]
	assert.True(t, ok, "Could not find iam capability when expected; got capabilities %v", capabilities)
}

func TestCapabilitiesTaskIAMRoleForUnSupportedDockerVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	conf := &config.Config{
		TaskIAMRoleEnabled: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_18,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["com.amazonaws.ecs.capability.task-iam-role"]
	assert.False(t, ok, "task-iam-role capability set for unsupported docker version")
}

func TestCapabilitiesTaskIAMRoleNetworkHostForSupportedDockerVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	conf := &config.Config{
		TaskIAMRoleEnabledForNetworkHost: true,
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_19,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["com.amazonaws.ecs.capability.task-iam-role-network-host"]
	assert.True(t, ok, "Could not find iam capability when expected; got capabilities %v", capabilities)
}

func TestCapabilitiesTaskIAMRoleNetworkHostForUnSupportedDockerVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	conf := &config.Config{
		TaskIAMRoleEnabledForNetworkHost: true,
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_18,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["com.amazonaws.ecs.capability.task-iam-role-network-host"]
	assert.False(t, ok, "task-iam-role capability set for unsupported docker version")
}

func TestAWSVPCBlockInstanceMetadataWhenTaskENIIsDisabled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	cniClient := mock_ecscni.NewMockCNIClient(ctrl)
	mockCredentialsProvider := app_mocks.NewMockCredentialsProvider(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	conf := &config.Config{
		AvailableLoggingDrivers: []dockerclient.LoggingDriver{
			dockerclient.JSONFileDriver,
		},
		TaskENIEnabled:             config.BooleanDefaultFalse{Value: config.ExplicitlyDisabled},
		AWSVPCBlockInstanceMetdata: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
			dockerclient.Version_1_17,
			dockerclient.Version_1_18,
		}),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)

	expectedCapabilityNames := []string{
		capabilityPrefix + "privileged-container",
		capabilityPrefix + "docker-remote-api.1.17",
		capabilityPrefix + "docker-remote-api.1.18",
		capabilityPrefix + "logging-driver.json-file",
	}

	var expectedCapabilities []types.Attribute
	for _, name := range expectedCapabilityNames {
		expectedCapabilities = append(expectedCapabilities,
			types.Attribute{Name: aws.String(name)})
	}

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		cniClient:             cniClient,
		pauseLoader:           mockPauseLoader,
		credentialsCache:      aws.NewCredentialsCache(mockCredentialsProvider),
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}
	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	for _, expected := range expectedCapabilities {
		assert.Contains(t, capabilities, types.Attribute{
			Name:  expected.Name,
			Value: expected.Value,
		})
	}

	for _, capability := range capabilities {
		if aws.ToString(capability.Name) == "ecs.capability.task-eni-block-instance-metadata" {
			t.Errorf("%s capability found when Task ENI is disabled in the config", taskENIBlockInstanceMetadataAttributeSuffix)
		}
	}
}

func TestCapabilitiesExecutionRoleAWSLogs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	cniClient := mock_ecscni.NewMockCNIClient(ctrl)
	conf := &config.Config{
		OverrideAWSLogsExecutionRole: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskENIEnabled:               config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
	}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_17,
	})
	// CNI plugins are platform dependent.
	// Therefore, for any version query for any plugin return an error
	cniClient.EXPECT().Version(gomock.Any()).Return("v1", errors.New("some error happened"))
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		cniClient:             cniClient,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	require.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["ecs.capability.execution-role-awslogs"]
	assert.True(t, ok, "Could not find AWSLogs execution role capability when expected; got capabilities %v", capabilities)
}

func TestCapabilitiesTaskResourceLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	conf := &config.Config{TaskCPUMemLimit: config.BooleanDefaultTrue{Value: config.ExplicitlyEnabled}}

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	versionList := []dockerclient.DockerVersion{dockerclient.Version_1_22}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return(versionList),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	expectedCapability := attributePrefix + capabilityTaskCPUMemLimit

	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap[expectedCapability]
	assert.True(t, ok, "Should contain: "+expectedCapability)
	assert.True(t, agent.cfg.TaskCPUMemLimit.Enabled(), "TaskCPUMemLimit should remain true")
}

func TestCapabilitesTaskResourceLimitDisabledByMissingDockerVersion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	conf := &config.Config{TaskCPUMemLimit: config.BooleanDefaultTrue{Value: config.NotSet}}

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	versionList := []dockerclient.DockerVersion{dockerclient.Version_1_19}
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return(versionList),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	unexpectedCapability := attributePrefix + capabilityTaskCPUMemLimit
	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap[unexpectedCapability]

	assert.False(t, ok, "Docker 1.22 is required for task resource limits. Should be disabled")
	assert.False(t, conf.TaskCPUMemLimit.Enabled(), "TaskCPUMemLimit should be made false when we can't find the right docker.")
}

func TestCapabilitesTaskResourceLimitErrorCase(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	conf := &config.Config{TaskCPUMemLimit: config.BooleanDefaultTrue{Value: config.ExplicitlyEnabled}}

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	versionList := []dockerclient.DockerVersion{dockerclient.Version_1_19}
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return(versionList),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		pauseLoader:           mockPauseLoader,
		dockerClient:          client,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	assert.Nil(t, capabilities)
	assert.Error(t, err, "An error should be thrown when TaskCPUMemLimit is explicitly enabled")
}

func TestCapabilitiesIncreasedTaskCPULimit(t *testing.T) {
	testCases := []struct {
		testName                             string
		taskCPUMemLimitValue                 config.Conditional
		dockerVersion                        dockerclient.DockerVersion
		expectedIncreasedTaskCPULimitEnabled bool
	}{
		{
			testName:                             "enabled by default",
			taskCPUMemLimitValue:                 config.NotSet,
			dockerVersion:                        dockerclient.Version_1_22,
			expectedIncreasedTaskCPULimitEnabled: true,
		},
		{
			testName:                             "disabled, unsupportedDockerVersion",
			taskCPUMemLimitValue:                 config.NotSet,
			dockerVersion:                        dockerclient.Version_1_19,
			expectedIncreasedTaskCPULimitEnabled: false,
		},
		{
			testName:                             "disabled, taskCPUMemLimit explicitly disabled",
			taskCPUMemLimitValue:                 config.ExplicitlyDisabled,
			dockerVersion:                        dockerclient.Version_1_22,
			expectedIncreasedTaskCPULimitEnabled: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			conf := &config.Config{
				TaskCPUMemLimit: config.BooleanDefaultTrue{Value: tc.taskCPUMemLimitValue},
			}

			client := mock_dockerapi.NewMockDockerClient(ctrl)
			versionList := []dockerclient.DockerVersion{tc.dockerVersion}
			mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
			mockPauseLoader := mock_loader.NewMockLoader(ctrl)
			mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
			mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
			mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
			mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
			mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

			mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
			mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
			mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

			gomock.InOrder(
				client.EXPECT().SupportedVersions().Return(versionList),
				mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
				client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
					gomock.Any()).AnyTimes().Return([]string{}, nil),
			)
			ctx, cancel := context.WithCancel(context.TODO())
			// Cancel the context to cancel async routines
			defer cancel()
			agent := &ecsAgent{
				ctx:                   ctx,
				cfg:                   conf,
				dockerClient:          client,
				pauseLoader:           mockPauseLoader,
				mobyPlugins:           mockMobyPlugins,
				serviceconnectManager: mockServiceConnectManager,
				daemonManagers:        mockDaemonManagers,
			}

			capability := attributePrefix + capabilityIncreasedTaskCPULimit
			capabilities, err := agent.capabilities()
			assert.NoError(t, err)

			capMap := make(map[string]bool)
			for _, capability := range capabilities {
				capMap[aws.ToString(capability.Name)] = true
			}

			_, ok := capMap[capability]
			assert.Equal(t, tc.expectedIncreasedTaskCPULimitEnabled, ok)
		})
	}
}

func TestCapabilitiesContainerHealth(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_24,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   &config.Config{},
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	require.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	_, ok := capMap["ecs.capability.container-health-check"]
	assert.True(t, ok, "Could not find container health check capability when expected; got capabilities %v", capabilities)
}

func TestCapabilitiesContainerHealthDisabled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
		dockerclient.Version_1_24,
	})
	mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil)
	client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any()).AnyTimes().Return([]string{}, nil)

	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   &config.Config{DisableDockerHealthCheck: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled}},
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	require.NoError(t, err)

	capMap := make(map[string]bool)
	for _, capability := range capabilities {
		capMap[aws.ToString(capability.Name)] = true
	}

	assert.NotContains(t, "ecs.capability.container-health-check", "Find container health check capability unexpected when it is disabled")
}

func TestCapabilitesListPluginsErrorCase(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	versionList := []dockerclient.DockerVersion{dockerclient.Version_1_19}
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return(versionList),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return(nil, errors.New("listPlugins error happened")),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   &config.Config{},
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	require.NoError(t, err)

	for _, capability := range capabilities {
		if strings.HasPrefix(aws.ToString(capability.Name), "ecs.capability.docker-volume-driver") {
			assert.Equal(t, aws.ToString(capability.Name), "ecs.capability.docker-volume-driver.local")
		}
	}
}

func TestCapabilitesScanPluginsErrorCase(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	versionList := []dockerclient.DockerVersion{dockerclient.Version_1_19}
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return(versionList),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return(nil, errors.New("Scan plugins error happened")),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)
	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   &config.Config{},
		dockerClient:          client,
		pauseLoader:           mockPauseLoader,
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}

	capabilities, err := agent.capabilities()
	require.NoError(t, err)

	for _, capability := range capabilities {
		if strings.HasPrefix(aws.ToString(capability.Name), "ecs.capability.docker-volume-driver") {
			assert.Equal(t, aws.ToString(capability.Name), "ecs.capability.docker-volume-driver.local")
		}
	}
}

func TestCapabilitiesExecuteCommand(t *testing.T) {
	execCapability := types.Attribute{
		Name: aws.String(attributePrefix + capabilityExec),
	}
	testCases := []struct {
		name                     string
		pathExists               func(string, bool) (bool, error)
		getSubDirectories        func(path string) ([]string, error)
		invalidSsmVersions       map[string]struct{}
		shouldHaveExecCapability bool
		osPlatformNotSupported   bool
	}{
		{
			name:                     "execute-command capability should not be added if any required file is not found",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return false, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"3.0.236.0"}, nil },
			shouldHaveExecCapability: false,
		},
		{
			name:                     "execute-command capability should not be added if no ssm versions are found",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return nil, nil },
			shouldHaveExecCapability: false,
		},
		{
			name:                     "execute-command capability should not be added if no valid ssm versions are found",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"2.0.0.0", "some_folder"}, nil },
			invalidSsmVersions:       map[string]struct{}{"2.0.0.0": struct{}{}},
			shouldHaveExecCapability: false,
		},
		{
			name:                     "execute-command capability should not be added if there are directroies exist but have no valid ssm version exist",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"3.0.236.0", "3.1.23.0"}, nil },
			invalidSsmVersions:       map[string]struct{}{"3.0.236.0": struct{}{}, "3.1.23.0": struct{}{}},
			shouldHaveExecCapability: false,
		},
		{
			name:                     "execute-command capability should be added if requirements are met",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"3.0.236.0"}, nil },
			shouldHaveExecCapability: true,
		},
		{
			name:                     "execute-command capability should be added if have valid ssm version exists",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"3.0.236.0", "3.1.23.0"}, nil },
			shouldHaveExecCapability: true,
		},
		{
			name:                     "execute-command capability should not be added if os platform is not supported",
			pathExists:               func(path string, shouldBeDirectory bool) (bool, error) { return true, nil },
			getSubDirectories:        func(path string) ([]string, error) { return []string{"3.0.236.0"}, nil },
			osPlatformNotSupported:   true,
			shouldHaveExecCapability: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pathExists = tc.pathExists
			getSubDirectories = tc.getSubDirectories
			oCapabilityExecInvalidSsmVersions := capabilityExecInvalidSsmVersions
			capabilityExecInvalidSsmVersions = tc.invalidSsmVersions
			isPlatformExecSupported = func() (bool, error) { return !tc.osPlatformNotSupported, nil }
			defer func() {
				mockPathExists(false)
				getSubDirectories = defaultGetSubDirectories
				capabilityExecInvalidSsmVersions = oCapabilityExecInvalidSsmVersions
				isPlatformExecSupported = defaultIsPlatformExecSupported
			}()
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
			client := mock_dockerapi.NewMockDockerClient(ctrl)
			versionList := []dockerclient.DockerVersion{dockerclient.Version_1_19}
			mockPauseLoader := mock_loader.NewMockLoader(ctrl)
			mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
			mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
			mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(true, nil).AnyTimes()
			mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
			mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

			mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
			mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
			mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

			gomock.InOrder(
				client.EXPECT().SupportedVersions().Return(versionList),
				mockMobyPlugins.EXPECT().Scan().AnyTimes().Return(nil, errors.New("Scan plugins error happened")),
				client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
					gomock.Any()).AnyTimes().Return([]string{}, nil),
			)
			ctx, cancel := context.WithCancel(context.TODO())
			// Cancel the context to cancel async routines
			defer cancel()
			agent := &ecsAgent{
				ctx:                   ctx,
				cfg:                   &config.Config{},
				dockerClient:          client,
				pauseLoader:           mockPauseLoader,
				mobyPlugins:           mockMobyPlugins,
				serviceconnectManager: mockServiceConnectManager,
				daemonManagers:        mockDaemonManagers,
			}

			capabilities, err := agent.capabilities()
			if err != nil {
				t.Fatal(err)
			}

			if tc.shouldHaveExecCapability {
				assert.Contains(t, capabilities, execCapability)
			} else {
				assert.NotContains(t, capabilities, execCapability)
			}
		})
	}
}

// TODO add test for no EBS Task Attach capability

func TestCapabilitiesNoServiceConnect(t *testing.T) {
	mockPathExists(true)
	defer mockPathExists(false)
	getSubDirectories = func(path string) ([]string, error) {
		// appendExecCapabilities() requires at least 1 version to exist
		return []string{"3.0.236.0"}, nil
	}
	defer func() {
		getSubDirectories = defaultGetSubDirectories
	}()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_dockerapi.NewMockDockerClient(ctrl)
	cniClient := mock_ecscni.NewMockCNIClient(ctrl)
	mockCredentialsProvider := app_mocks.NewMockCredentialsProvider(ctrl)
	mockMobyPlugins := mock_mobypkgwrapper.NewMockPlugins(ctrl)
	mockPauseLoader := mock_loader.NewMockLoader(ctrl)
	conf := &config.Config{
		AvailableLoggingDrivers: []dockerclient.LoggingDriver{
			dockerclient.JSONFileDriver,
			dockerclient.SyslogDriver,
			dockerclient.JournaldDriver,
			dockerclient.GelfDriver,
			dockerclient.FluentdDriver,
		},
		PrivilegedDisabled:         config.BooleanDefaultFalse{Value: config.ExplicitlyDisabled},
		SELinuxCapable:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AppArmorCapable:            config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskENIEnabled:             config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		AWSVPCBlockInstanceMetdata: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		TaskCleanupWaitDuration:    config.DefaultConfig(ipcompatibility.NewIPv4OnlyCompatibility()).TaskCleanupWaitDuration,
	}

	mockPauseLoader.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()

	mockServiceConnectManager := mock_serviceconnect.NewMockManager(ctrl)
	mockServiceConnectManager.EXPECT().IsLoaded(gomock.Any()).Return(false, nil).AnyTimes()
	mockServiceConnectManager.EXPECT().LoadImage(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("No File")).AnyTimes()
	mockServiceConnectManager.EXPECT().GetLoadedAppnetVersion().AnyTimes()
	mockServiceConnectManager.EXPECT().GetCapabilitiesForAppnetInterfaceVersion("").AnyTimes()

	mockDaemonManager := mock_daemonmanager.NewMockDaemonManager(ctrl)
	mockDaemonManagers := map[string]dm.DaemonManager{md.EbsCsiDriver: mockDaemonManager}
	mockDaemonManager.EXPECT().LoadImage(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	// Scan() and ListPluginsWithFilters() are tested with
	// AnyTimes() because they are not called in windows.
	gomock.InOrder(
		client.EXPECT().SupportedVersions().Return([]dockerclient.DockerVersion{
			dockerclient.Version_1_17,
			dockerclient.Version_1_18,
			dockerclient.Version_1_19,
		}),
		// CNI plugins are platform dependent.
		// Therefore, for any version query for any plugin return an appropriate version
		cniClient.EXPECT().Version(gomock.Any()).Return("v1", nil),
		mockMobyPlugins.EXPECT().Scan().AnyTimes().Return([]string{}, nil),
		client.EXPECT().ListPluginsWithFilters(gomock.Any(), gomock.Any(), gomock.Any(),
			gomock.Any()).AnyTimes().Return([]string{}, nil),
	)

	expectedNameOnlyCapabilities := []string{
		capabilityPrefix + "privileged-container",
		capabilityPrefix + "docker-remote-api.1.17",
		capabilityPrefix + "docker-remote-api.1.18",
		capabilityPrefix + "logging-driver.json-file",
		capabilityPrefix + "logging-driver.syslog",
		capabilityPrefix + "logging-driver.journald",
		capabilityPrefix + "selinux",
		capabilityPrefix + "apparmor",
		attributePrefix + "docker-plugin.local",
		attributePrefix + taskENIAttributeSuffix,
		attributePrefix + capabilityPrivateRegistryAuthASM,
		attributePrefix + capabilitySecretEnvSSM,
		attributePrefix + capabilitySecretLogDriverSSM,
		attributePrefix + capabilityECREndpoint,
		attributePrefix + capabilitySecretEnvASM,
		attributePrefix + capabilitySecretLogDriverASM,
		attributePrefix + capabilityContainerOrdering,
		attributePrefix + capabilityFullTaskSync,
		attributePrefix + capabilityEnvFilesS3,
		attributePrefix + taskENIBlockInstanceMetadataAttributeSuffix,
		attributePrefix + capabilityExec,
		attributePrefix + capabilityContainerPortRange,
		attributePrefix + capabilityContainerRestartPolicy,
	}

	var expectedCapabilities []types.Attribute
	for _, name := range expectedNameOnlyCapabilities {
		expectedCapabilities = append(expectedCapabilities,
			types.Attribute{Name: aws.String(name)})
	}
	expectedCapabilities = append(expectedCapabilities,
		[]types.Attribute{
			{
				Name:  aws.String(attributePrefix + cniPluginVersionSuffix),
				Value: aws.String("v1"),
			},
		}...)

	ctx, cancel := context.WithCancel(context.TODO())
	// Cancel the context to cancel async routines
	defer cancel()
	agent := &ecsAgent{
		ctx:                   ctx,
		cfg:                   conf,
		dockerClient:          client,
		cniClient:             cniClient,
		pauseLoader:           mockPauseLoader,
		credentialsCache:      aws.NewCredentialsCache(mockCredentialsProvider),
		mobyPlugins:           mockMobyPlugins,
		serviceconnectManager: mockServiceConnectManager,
		daemonManagers:        mockDaemonManagers,
	}
	capabilities, err := agent.capabilities()
	assert.NoError(t, err)

	for _, expected := range expectedCapabilities {
		assert.Contains(t, capabilities, types.Attribute{
			Name:  expected.Name,
			Value: expected.Value,
		})
	}
}

func TestDefaultGetSubDirectories(t *testing.T) {
	rootDir := t.TempDir()

	subDir, err := ioutil.TempDir(rootDir, "dir")
	if err != nil {
		t.Fatal(err)
	}
	file, err := ioutil.TempFile(rootDir, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer require.NoError(t, file.Close())
	notExistingPath := filepath.Join(rootDir, "not-existing")

	testCases := []struct {
		name           string
		path           string
		expectedResult []string
		shouldFail     bool
	}{
		{
			name:           "return names of child folders if path exists",
			path:           rootDir,
			expectedResult: []string{filepath.Base(subDir)},
			shouldFail:     false,
		},
		{
			name:           "return error if path does not exist",
			path:           notExistingPath,
			expectedResult: nil,
			shouldFail:     true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			subDirectories, err := defaultGetSubDirectories(tc.path)
			if tc.shouldFail {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				// actual result should have the same elements, don't need to be in same order
				assert.Subset(t, tc.expectedResult, subDirectories)
				assert.Subset(t, subDirectories, tc.expectedResult)
			}
		})
	}
}

func TestDefaultPathExistsd(t *testing.T) {
	rootDir := t.TempDir()

	file, err := ioutil.TempFile(rootDir, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer require.NoError(t, file.Close())
	notExistingPath := filepath.Join(rootDir, "not-existing")
	testCases := []struct {
		name              string
		path              string
		shouldBeDirectory bool
		expected          bool
	}{
		{
			name:              "return false if directory does not exist",
			path:              notExistingPath,
			shouldBeDirectory: true,
			expected:          false,
		},
		{
			name:              "return false if false does not exist",
			path:              notExistingPath,
			shouldBeDirectory: false,
			expected:          false,
		},
		{
			name:              "if directory exists, return shouldBeDirectory",
			path:              rootDir,
			shouldBeDirectory: true,
			expected:          true,
		},
		{
			name:              "if directory exists, return shouldBeDirectory",
			path:              rootDir,
			shouldBeDirectory: false,
			expected:          false,
		},
		{
			name:              "if file exists, return !shouldBeDirectory",
			path:              file.Name(),
			shouldBeDirectory: false,
			expected:          true,
		},
		{
			name:              "if file exists, return !shouldBeDirectory",
			path:              file.Name(),
			shouldBeDirectory: true,
			expected:          false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := defaultPathExists(tc.path, tc.shouldBeDirectory)
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, result, tc.expected)
		})
	}
}

func TestAppendAndRemoveAttributes(t *testing.T) {
	attrs := appendNameOnlyAttribute([]types.Attribute{}, "cap-1")
	attrs = appendNameOnlyAttribute(attrs, "cap-2")
	require.Len(t, attrs, 2)
	assert.Contains(t, attrs, types.Attribute{
		Name: aws.String("cap-1"),
	})
	assert.Contains(t, attrs, types.Attribute{
		Name: aws.String("cap-2"),
	})

	attrs = removeAttributesByNames(attrs, []string{"cap-1"})
	require.Len(t, attrs, 1)
	assert.NotContains(t, attrs, types.Attribute{
		Name: aws.String("cap-1"),
	})
	assert.Contains(t, attrs, types.Attribute{
		Name: aws.String("cap-2"),
	})
}

func TestAppendGMSACapabilities(t *testing.T) {
	var inputCapabilities []types.Attribute
	var expectedCapabilities []types.Attribute

	expectedCapabilities = append(expectedCapabilities,
		[]types.Attribute{
			{
				Name: aws.String(attributePrefix + capabilityGMSA),
			},
		}...)

	agent := &ecsAgent{
		cfg: &config.Config{
			GMSACapable: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		},
	}

	capabilities := agent.appendGMSACapabilities(inputCapabilities)

	assert.Equal(t, len(expectedCapabilities), len(capabilities))
	for i, expected := range expectedCapabilities {
		assert.Equal(t, aws.ToString(expected.Name), aws.ToString(capabilities[i].Name))
		assert.Equal(t, aws.ToString(expected.Value), aws.ToString(capabilities[i].Value))
	}
}

func TestAppendGMSADomainlessCapabilities(t *testing.T) {
	var inputCapabilities []types.Attribute
	var expectedCapabilities []types.Attribute

	expectedCapabilities = append(expectedCapabilities,
		[]types.Attribute{
			{
				Name: aws.String(attributePrefix + capabilityGMSADomainless),
			},
		}...)

	agent := &ecsAgent{
		cfg: &config.Config{
			GMSADomainlessCapable: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
		},
	}

	capabilities := agent.appendGMSADomainlessCapabilities(inputCapabilities)

	assert.Equal(t, len(expectedCapabilities), len(capabilities))
	for i, expected := range expectedCapabilities {
		assert.Equal(t, aws.ToString(expected.Name), aws.ToString(capabilities[i].Name))
		assert.Equal(t, aws.ToString(expected.Value), aws.ToString(capabilities[i].Value))
	}
}

func TestAppendGMSADomainlessCapabilitiesFalse(t *testing.T) {
	var inputCapabilities []types.Attribute
	var expectedCapabilities []types.Attribute

	expectedCapabilities = append(expectedCapabilities,
		[]types.Attribute{}...)

	agent := &ecsAgent{
		cfg: &config.Config{
			GMSADomainlessCapable: config.BooleanDefaultFalse{Value: config.ExplicitlyDisabled},
		},
	}

	capabilities := agent.appendGMSADomainlessCapabilities(inputCapabilities)

	assert.Equal(t, len(expectedCapabilities), len(capabilities))
}

func TestAppendFaultInjectionCapabilities(t *testing.T) {
	originalIsFaultInjectionToolingAvailable := isFaultInjectionToolingAvailable
	defer func() { isFaultInjectionToolingAvailable = originalIsFaultInjectionToolingAvailable }()
	t.Run("Fault Injection Capability Available", func(t *testing.T) {
		// Test case where required tooling is available
		isFaultInjectionToolingAvailable = func(cfg *config.Config) bool { return true }
		capabilities := []types.Attribute{}
		agent := &ecsAgent{
			cfg: &config.Config{},
		}
		capabilities = agent.appendFaultInjectionCapabilities(capabilities)
		// Check that the only capability is "ecs.capability.fault-injection"
		require.Len(t, capabilities, 1)
		assert.Equal(t, "ecs.capability.fault-injection", aws.ToString(capabilities[0].Name))
	})
	t.Run("Fault Injection Capability Not Available", func(t *testing.T) {
		// Test case where required tooling is not available
		isFaultInjectionToolingAvailable = func(cfg *config.Config) bool { return false }
		capabilities := []types.Attribute{}
		agent := &ecsAgent{
			cfg: &config.Config{},
		}
		capabilities = agent.appendFaultInjectionCapabilities(capabilities)
		// Check that no capability is added
		assert.Empty(t, capabilities)
	})

	t.Run("Fault Injection Capability Not Available for EXTERNAL Launch Type", func(t *testing.T) {
		// Test case where required tooling is available but EXTERNAL Launch Type
		isFaultInjectionToolingAvailable = func(cfg *config.Config) bool { return true }
		capabilities := []types.Attribute{}
		agent := &ecsAgent{
			cfg: &config.Config{
				External: config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled},
			},
		}
		capabilities = agent.appendFaultInjectionCapabilities(capabilities)
		assert.Empty(t, capabilities)
	})
}
