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

package api

import (
	"fmt"
	"strconv"
	"time"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/statechange"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/attachment"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/ecs"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger/field"
	ni "github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/networkinterface"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
)

const (
	// ecsMaxNetworkBindingsLength is the maximum length of the ecs.NetworkBindings list sent as part of the
	// container state change payload. Currently, this is enforced only when containerPortRanges are requested.
	ecsMaxNetworkBindingsLength = 100
)

// ContainerStateChange represents a state change that needs to be sent to the
// SubmitContainerStateChange API
type ContainerStateChange struct {
	// TaskArn is the unique identifier for the task
	TaskArn string
	// RuntimeID is the dockerID of the container
	RuntimeID string
	// ContainerName is the name of the container
	ContainerName string
	// Status is the status to send
	Status apicontainerstatus.ContainerStatus
	// ImageDigest is the sha-256 digest of the container image as pulled from the repository
	ImageDigest string
	// Reason may contain details of why the container stopped
	Reason string
	// ExitCode is the exit code of the container, if available
	ExitCode *int
	// PortBindings are the details of the host ports picked for the specified
	// container ports
	PortBindings []apicontainer.PortBinding
	// Container is a pointer to the container involved in the state change that gives the event handler a hook into
	// storing what status was sent.  This is used to ensure the same event is handled only once.
	Container *apicontainer.Container
}

type ManagedAgentStateChange struct {
	// TaskArn is the unique identifier for the task
	TaskArn string
	// Name is the name of the managed agent
	Name string
	// Container is a pointer to the container involved in the state change that gives the event handler a hook into
	// storing what status was sent.  This is used to ensure the same event is handled only once.
	Container *apicontainer.Container
	// Status is the status of the managed agent
	Status apicontainerstatus.ManagedAgentStatus
	// Reason indicates an error in a managed agent state chage
	Reason string
}

// TaskStateChange represents a state change that needs to be sent to the
// SubmitTaskStateChange API
type TaskStateChange struct {
	// Attachment is the eni attachment object to send
	Attachment *ni.ENIAttachment
	// TaskArn is the unique identifier for the task
	TaskARN string
	// Status is the status to send
	Status apitaskstatus.TaskStatus
	// Reason may contain details of why the task stopped
	Reason string
	// Containers holds the events generated by containers owned by this task
	Containers []ContainerStateChange
	// ManagedAgents contain the name and status of Agents running inside the container
	ManagedAgents []ManagedAgentStateChange
	// PullStartedAt is the timestamp when the task start pulling
	PullStartedAt *time.Time
	// PullStoppedAt is the timestamp when the task finished pulling
	PullStoppedAt *time.Time
	// ExecutionStoppedAt is the timestamp when the essential container stopped
	ExecutionStoppedAt *time.Time
	// Task is a pointer to the task involved in the state change that gives the event handler a hook into storing
	// what status was sent.  This is used to ensure the same event is handled only once.
	Task *apitask.Task
}

// AttachmentStateChange represents a state change that needs to be sent to the
// SubmitAttachmentStateChanges API
type AttachmentStateChange struct {
	// Attachment is the attachment object to send
	Attachment attachment.Attachment
}

type ErrShouldNotSendEvent struct {
	resourceId string
}

func (e ErrShouldNotSendEvent) Error() string {
	return fmt.Sprintf("should not send events for internal tasks or containers: %s", e.resourceId)
}

// NewTaskStateChangeEvent creates a new task state change event
// returns error if the state change doesn't need to be sent to the ECS backend.
func NewTaskStateChangeEvent(task *apitask.Task, reason string) (TaskStateChange, error) {
	var event TaskStateChange
	if task.IsInternal {
		return event, ErrShouldNotSendEvent{task.Arn}
	}
	taskKnownStatus := task.GetKnownStatus()
	if taskKnownStatus != apitaskstatus.TaskManifestPulled && !taskKnownStatus.BackendRecognized() {
		return event, errors.Errorf(
			"create task state change event api: status not recognized by ECS: %v",
			taskKnownStatus)
	}
	if task.GetSentStatus() >= taskKnownStatus {
		return event, errors.Errorf(
			"create task state change event api: status [%s] already sent",
			taskKnownStatus.String())
	}
	if taskKnownStatus == apitaskstatus.TaskManifestPulled && !task.HasAContainerWithResolvedDigest() {
		return event, ErrShouldNotSendEvent{
			fmt.Sprintf(
				"create task state change event api: status %s not eligible for backend reporting as"+
					" no digests were resolved",
				apitaskstatus.TaskManifestPulled.String()),
		}
	}

	event = TaskStateChange{
		TaskARN: task.Arn,
		Status:  taskKnownStatus,
		Reason:  reason,
		Task:    task,
	}

	event.SetTaskTimestamps()

	return event, nil
}

// NewContainerStateChangeEvent creates a new container state change event
// returns error if the state change doesn't need to be sent to the ECS backend.
func NewContainerStateChangeEvent(task *apitask.Task, cont *apicontainer.Container, reason string) (ContainerStateChange, error) {
	event, err := newUncheckedContainerStateChangeEvent(task, cont, reason)
	if err != nil {
		return event, err
	}
	contKnownStatus := cont.GetKnownStatus()
	if contKnownStatus != apicontainerstatus.ContainerManifestPulled &&
		!contKnownStatus.ShouldReportToBackend(cont.GetSteadyStateStatus()) {
		return event, ErrShouldNotSendEvent{fmt.Sprintf(
			"create container state change event api: status not recognized by ECS: %v",
			contKnownStatus)}
	}
	if contKnownStatus == apicontainerstatus.ContainerManifestPulled && !cont.DigestResolved() {
		// Transition to MANIFEST_PULLED state is sent to the backend only to report a resolved
		// image manifest digest. No need to generate an event if the digest was not resolved
		// which could happen due to various reasons.
		return event, ErrShouldNotSendEvent{fmt.Sprintf(
			"create container state change event api:"+
				" no need to send %s event as no resolved digests were found",
			apicontainerstatus.ContainerManifestPulled.String())}
	}
	if cont.GetSentStatus() >= contKnownStatus {
		return event, ErrShouldNotSendEvent{fmt.Sprintf(
			"create container state change event api: status [%s] already sent for container %s, task %s",
			contKnownStatus.String(), cont.Name, task.Arn)}
	}
	if reason == "" && cont.ApplyingError != nil {
		reason = cont.ApplyingError.Error()
		event.Reason = reason
	}
	return event, nil
}

func newUncheckedContainerStateChangeEvent(task *apitask.Task, cont *apicontainer.Container, reason string) (ContainerStateChange, error) {
	var event ContainerStateChange
	if cont.IsInternal() {
		return event, ErrShouldNotSendEvent{cont.Name}
	}
	portBindings := cont.GetKnownPortBindings()
	if task.IsServiceConnectEnabled() && task.IsNetworkModeBridge() {
		pauseCont, err := task.GetBridgeModePauseContainerForTaskContainer(cont)
		if err != nil {
			return event, fmt.Errorf("error resolving pause container for bridge mode SC container: %s", cont.Name)
		}
		portBindings = pauseCont.GetKnownPortBindings()
	}
	contKnownStatus := cont.GetKnownStatus()
	event = ContainerStateChange{
		TaskArn:       task.Arn,
		ContainerName: cont.Name,
		RuntimeID:     cont.GetRuntimeID(),
		Status:        containerStatusChangeStatus(contKnownStatus, cont.GetSteadyStateStatus()),
		ExitCode:      cont.GetKnownExitCode(),
		PortBindings:  portBindings,
		ImageDigest:   cont.GetImageDigest(),
		Reason:        reason,
		Container:     cont,
	}
	return event, nil
}

// Maps container known status to a suitable status for ContainerStateChange.
//
// Returns ContainerRunning if known status matches steady state status,
// returns knownStatus if it is ContainerManifestPulled or ContainerStopped,
// returns ContainerStatusNone for all other cases.
func containerStatusChangeStatus(
	knownStatus apicontainerstatus.ContainerStatus,
	steadyStateStatus apicontainerstatus.ContainerStatus,
) apicontainerstatus.ContainerStatus {
	switch knownStatus {
	case steadyStateStatus:
		return apicontainerstatus.ContainerRunning
	case apicontainerstatus.ContainerManifestPulled:
		return apicontainerstatus.ContainerManifestPulled
	case apicontainerstatus.ContainerStopped:
		return apicontainerstatus.ContainerStopped
	default:
		return apicontainerstatus.ContainerStatusNone
	}
}

// NewManagedAgentChangeEvent creates a new managedAgent change event to convey managed agent state changes
// returns error if the state change doesn't need to be sent to the ECS backend.
func NewManagedAgentChangeEvent(task *apitask.Task, cont *apicontainer.Container, managedAgentName string, reason string) (ManagedAgentStateChange, error) {
	var event = ManagedAgentStateChange{}
	managedAgent, ok := cont.GetManagedAgentByName(managedAgentName)
	if !ok {
		return event, errors.Errorf("No ExecuteCommandAgent available in container: %v", cont.Name)
	}
	if !managedAgent.Status.ShouldReportToBackend() {
		return event, errors.Errorf("create managed agent state change event: status not recognized by ECS: %v", managedAgent.Status)
	}

	event = ManagedAgentStateChange{
		TaskArn:   task.Arn,
		Name:      managedAgent.Name,
		Container: cont,
		Status:    managedAgent.Status,
		Reason:    reason,
	}

	return event, nil
}

// NewAttachmentStateChangeEvent creates a new attachment state change event
func NewAttachmentStateChangeEvent(eniAttachment *ni.ENIAttachment) AttachmentStateChange {
	return AttachmentStateChange{
		Attachment: eniAttachment,
	}
}

func (c *ContainerStateChange) ToFields() logger.Fields {
	return logger.Fields{
		"eventType":       "ContainerStateChange",
		"taskArn":         c.TaskArn,
		"containerName":   c.ContainerName,
		"containerStatus": c.Status.String(),
		"exitCode":        strconv.Itoa(*c.ExitCode),
		"reason":          c.Reason,
		"portBindings":    c.PortBindings,
	}
}

// String returns a human readable string representation of this object
func (c *ContainerStateChange) String() string {
	res := fmt.Sprintf("containerName=%s containerStatus=%s", c.ContainerName, c.Status.String())
	if c.ExitCode != nil {
		res += " containerExitCode=" + strconv.Itoa(*c.ExitCode)
	}
	if c.Reason != "" {
		res += " containerReason=" + c.Reason
	}
	if len(c.PortBindings) != 0 {
		res += fmt.Sprintf(" containerPortBindings=%v", c.PortBindings)
	}
	if c.Container != nil {
		res += fmt.Sprintf(" containerKnownSentStatus=%s containerRuntimeID=%s containerIsEssential=%v",
			c.Container.GetSentStatus().String(), c.Container.GetRuntimeID(), c.Container.IsEssential())
	}
	return res
}

// ToECSAgent converts the agent module level ContainerStateChange to ecs-agent module level ContainerStateChange.
func (c *ContainerStateChange) ToECSAgent() (*ecs.ContainerStateChange, error) {
	pl, err := buildContainerStateChangePayload(*c)
	if err != nil {
		logger.Error("Could not convert agent container state change to ecs-agent container state change",
			logger.Fields{
				"agentContainerStateChange": c.String(),
				field.Error:                 err,
			})
		return nil, err
	} else if pl == nil {
		return nil, nil
	}

	return &ecs.ContainerStateChange{
		TaskArn:         c.TaskArn,
		RuntimeID:       aws.ToString(pl.RuntimeId),
		ContainerName:   c.ContainerName,
		Status:          c.Status,
		ImageDigest:     aws.ToString(pl.ImageDigest),
		Reason:          aws.ToString(pl.Reason),
		ExitCode:        utils.Int32PtrToIntPtr(pl.ExitCode),
		NetworkBindings: pl.NetworkBindings,
		MetadataGetter:  newContainerMetadataGetter(c.Container),
	}, nil
}

// String returns a human readable string representation of ManagedAgentStateChange
func (m *ManagedAgentStateChange) String() string {
	res := fmt.Sprintf("containerName=%s managedAgentName=%s managedAgentStatus=%s", m.Container.Name, m.Name, m.Status.String())
	if m.Reason != "" {
		res += " managedAgentReason=" + m.Reason
	}
	return res
}

// SetTaskTimestamps adds the timestamp information of task into the event
// to be sent by SubmitTaskStateChange
func (change *TaskStateChange) SetTaskTimestamps() {
	if change.Task == nil {
		return
	}

	// Send the task timestamp if set
	if timestamp := change.Task.GetPullStartedAt(); !timestamp.IsZero() {
		change.PullStartedAt = aws.Time(timestamp.UTC())
	}
	if timestamp := change.Task.GetPullStoppedAt(); !timestamp.IsZero() {
		change.PullStoppedAt = aws.Time(timestamp.UTC())
	}
	if timestamp := change.Task.GetExecutionStoppedAt(); !timestamp.IsZero() {
		change.ExecutionStoppedAt = aws.Time(timestamp.UTC())
	}
}

func (change *TaskStateChange) ToFields() logger.Fields {
	fields := logger.Fields{
		"eventType":  "TaskStateChange",
		"taskArn":    change.TaskARN,
		"taskStatus": change.Status.String(),
		"taskReason": change.Reason,
	}
	if change.Task != nil {
		fields["taskKnownSentStatus"] = change.Task.GetSentStatus().String()
		fields["taskPullStartedAt"] = change.Task.GetPullStartedAt().UTC().Format(time.RFC3339)
		fields["taskPullStoppedAt"] = change.Task.GetPullStoppedAt().UTC().Format(time.RFC3339)
		fields["taskExecutionStoppedAt"] = change.Task.GetExecutionStoppedAt().UTC().Format(time.RFC3339)
	}
	if change.Attachment != nil {
		fields["eniAttachment"] = change.Attachment.String()
	}
	for i, containerChange := range change.Containers {
		fields["containerChange-"+strconv.Itoa(i)] = containerChange.String()
	}
	for i, managedAgentChange := range change.ManagedAgents {
		fields["managedAgentChange-"+strconv.Itoa(i)] = managedAgentChange.String()
	}
	return fields
}

// String returns a human readable string representation of this object
func (change *TaskStateChange) String() string {
	res := fmt.Sprintf("%s -> %s", change.TaskARN, change.Status.String())
	if change.Task != nil {
		res += fmt.Sprintf(", Known Sent: %s, PullStartedAt: %s, PullStoppedAt: %s, ExecutionStoppedAt: %s",
			change.Task.GetSentStatus().String(),
			change.Task.GetPullStartedAt(),
			change.Task.GetPullStoppedAt(),
			change.Task.GetExecutionStoppedAt())
	}
	if change.Attachment != nil {
		res += ", " + change.Attachment.String()
	}
	for _, containerChange := range change.Containers {
		res += ", container change: " + containerChange.String()
	}
	for _, managedAgentChange := range change.ManagedAgents {
		res += ", managed agent: " + managedAgentChange.String()
	}

	return res
}

// ToECSAgent converts the agent module level TaskStateChange to ecs-agent module level TaskStateChange.
func (change *TaskStateChange) ToECSAgent() (*ecs.TaskStateChange, error) {
	output := &ecs.TaskStateChange{
		Attachment:         change.Attachment,
		TaskARN:            change.TaskARN,
		Status:             change.Status,
		Reason:             change.Reason,
		PullStartedAt:      change.PullStartedAt,
		PullStoppedAt:      change.PullStoppedAt,
		ExecutionStoppedAt: change.ExecutionStoppedAt,
		MetadataGetter:     newTaskMetadataGetter(change.Task),
	}

	for _, managedAgentEvent := range change.ManagedAgents {
		if mgspl := buildManagedAgentStateChangePayload(managedAgentEvent); mgspl != nil {
			output.ManagedAgents = append(output.ManagedAgents, *mgspl)
		}
	}

	containerEvents := make([]types.ContainerStateChange, len(change.Containers))
	for i, containerEvent := range change.Containers {
		payload, err := buildContainerStateChangePayload(containerEvent)
		if err != nil {
			logger.Error("Could not convert agent task state change to ecs-agent task state change", logger.Fields{
				"agentTaskStateChange": change.String(),
				field.Error:            err,
			})
			return nil, err
		}
		containerEvents[i] = *payload
	}
	output.Containers = containerEvents

	return output, nil
}

// String returns a human readable string representation of this object
func (change *AttachmentStateChange) String() string {
	if change.Attachment != nil {
		return fmt.Sprintf("%s -> %v, %s", change.Attachment.GetAttachmentARN(),
			change.Attachment.GetAttachmentStatus(), change.Attachment.String())
	}

	return ""
}

// ToECSAgent converts the agent module level AttachmentStateChange to ecs-agent module level AttachmentStateChange.
func (change *AttachmentStateChange) ToECSAgent() *ecs.AttachmentStateChange {
	return &ecs.AttachmentStateChange{
		Attachment: change.Attachment,
	}
}

// GetEventType returns an enum identifying the event type
func (ContainerStateChange) GetEventType() statechange.EventType {
	return statechange.ContainerEvent
}

func (ms ManagedAgentStateChange) GetEventType() statechange.EventType {
	return statechange.ManagedAgentEvent
}

// GetEventType returns an enum identifying the event type
func (ts TaskStateChange) GetEventType() statechange.EventType {
	return statechange.TaskEvent
}

// GetEventType returns an enum identifying the event type
func (AttachmentStateChange) GetEventType() statechange.EventType {
	return statechange.AttachmentEvent
}

func buildManagedAgentStateChangePayload(change ManagedAgentStateChange) *types.ManagedAgentStateChange {
	if !change.Status.ShouldReportToBackend() {
		logger.Warn("Not submitting unsupported managed agent state", logger.Fields{
			field.Status:        change.Status.String(),
			field.ContainerName: change.Container.Name,
			field.TaskARN:       change.TaskArn,
		})
		return nil
	}
	return &types.ManagedAgentStateChange{
		ManagedAgentName: types.ManagedAgentName(change.Name),
		ContainerName:    aws.String(change.Container.Name),
		Status:           aws.String(change.Status.String()),
		Reason:           aws.String(change.Reason),
	}
}

func buildContainerStateChangePayload(change ContainerStateChange) (*types.ContainerStateChange, error) {
	if change.ContainerName == "" {
		return nil, fmt.Errorf("container state change has no container name")
	}
	statechange := types.ContainerStateChange{
		ContainerName: aws.String(change.ContainerName),
	}
	if change.RuntimeID != "" {
		statechange.RuntimeId = aws.String(change.RuntimeID)
	}
	if change.Reason != "" {
		statechange.Reason = aws.String(change.Reason)
	}
	if change.ImageDigest != "" {
		statechange.ImageDigest = aws.String(change.ImageDigest)
	}

	// TODO: This check already exists in NewContainerStateChangeEvent and shouldn't be repeated here; remove after verifying
	stat := change.Status
	if stat != apicontainerstatus.ContainerManifestPulled &&
		stat != apicontainerstatus.ContainerStopped &&
		stat != apicontainerstatus.ContainerRunning {
		logger.Warn("Not submitting unsupported upstream container state", logger.Fields{
			field.Status:        stat.String(),
			field.ContainerName: change.ContainerName,
			field.TaskARN:       change.TaskArn,
		})
		return nil, nil
	}
	// TODO: This check is probably redundant as String() method never returns "DEAD"; remove after verifying
	if stat.String() == "DEAD" {
		stat = apicontainerstatus.ContainerStopped
	}
	statechange.Status = aws.String(stat.BackendStatusString())

	if change.ExitCode != nil {
		exitCode := int32(aws.ToInt(change.ExitCode))
		statechange.ExitCode = aws.Int32(exitCode)
	}

	networkBindings := getNetworkBindings(change)
	// we enforce a limit on the no. of network bindings for containers with at-least 1 port range requested.
	// this limit is enforced by ECS, and we fail early and don't call SubmitContainerStateChange.
	if change.Container.HasPortRange() && len(networkBindings) > ecsMaxNetworkBindingsLength {
		return nil, fmt.Errorf("no. of network bindings %v is more than the maximum supported no. %v, "+
			"container: %s "+"task: %s", len(networkBindings), ecsMaxNetworkBindingsLength, change.ContainerName, change.TaskArn)
	}
	statechange.NetworkBindings = networkBindings

	return &statechange, nil
}

// ProtocolBindIP used to store protocol and bindIP information associated to a particular host port
type ProtocolBindIP struct {
	protocol string
	bindIP   string
}

// getNetworkBindings returns the list of networkingBindings, sent to ECS as part of the container state change payload
func getNetworkBindings(change ContainerStateChange) []types.NetworkBinding {
	networkBindings := []types.NetworkBinding{}
	// hostPortToProtocolBindIPMap is a map to store protocol and bindIP information associated to host ports
	// that belong to a range. This is used in case when there are multiple protocol/bindIP combinations associated to a
	// port binding. example: when both IPv4 and IPv6 bindIPs are populated by docker.
	hostPortToProtocolBindIPMap := map[int64][]ProtocolBindIP{}

	// ContainerPortSet consists of singular ports, and ports that belong to a range, but for which we were not able to
	// find contiguous host ports and ask docker to pick instead.
	containerPortSet := change.Container.GetContainerPortSet()
	// each entry in the ContainerPortRangeMap implies that we found a contiguous host port range for the same
	containerPortRangeMap := change.Container.GetContainerPortRangeMap()

	for _, binding := range change.PortBindings {
		containerPort := int32(binding.ContainerPort)
		bindIP := binding.BindIP
		protocol := binding.Protocol.String()

		// create network binding for each containerPort that exists in the singular ContainerPortSet
		// for container ports that belong to a range, we'll have 1 consolidated network binding for the range
		if _, ok := containerPortSet[int(containerPort)]; ok {
			networkBindings = append(networkBindings, types.NetworkBinding{
				BindIP:        aws.String(bindIP),
				ContainerPort: aws.Int32(containerPort),
				HostPort:      aws.Int32(int32(binding.HostPort)),
				Protocol:      types.TransportProtocol(protocol),
			})
		} else {
			// populate hostPortToProtocolBindIPMap – this is used below when we construct network binding for ranges.
			hostPort := int64(binding.HostPort)
			hostPortToProtocolBindIPMap[hostPort] = append(hostPortToProtocolBindIPMap[hostPort],
				ProtocolBindIP{
					protocol: protocol,
					bindIP:   bindIP,
				})
		}
	}

	for containerPortRange, hostPortRange := range containerPortRangeMap {
		// we check for protocol and bindIP information associated to any one of the host ports from the hostPortRange,
		// all ports belonging to the same range share this information.
		hostPort, _, _ := nat.ParsePortRangeToInt(hostPortRange)
		if val, ok := hostPortToProtocolBindIPMap[int64(hostPort)]; ok {
			for _, v := range val {
				networkBindings = append(networkBindings, types.NetworkBinding{
					BindIP:             aws.String(v.bindIP),
					ContainerPortRange: aws.String(containerPortRange),
					HostPortRange:      aws.String(hostPortRange),
					Protocol:           types.TransportProtocol(v.protocol),
				})
			}
		}
	}

	return networkBindings
}
