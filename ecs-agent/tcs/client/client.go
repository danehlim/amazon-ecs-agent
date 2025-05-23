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

package tcsclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/doctor"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger/field"
	"github.com/aws/amazon-ecs-agent/ecs-agent/metrics"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils"
	"github.com/aws/amazon-ecs-agent/ecs-agent/wsclient"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/cihub/seelog"
	"github.com/pborman/uuid"
)

const (
	// tasksInMetricMessage is the maximum number of tasks that can be sent in a message to the backend
	// This is a very conservative estimate assuming max allowed string lengths for all fields.
	tasksInMetricMessage = 10
	// tasksInHealthMessage is the maximum number of tasks that can be sent in a message to the backend
	tasksInHealthMessage = 10
	// DefaultContainerMetricsPublishInterval is the default interval that we publish
	// metrics to the ECS telemetry backend (TACS)
	DefaultContainerMetricsPublishInterval = 20 * time.Second
)

var (
	// publishMetricRequestSizeLimit is the maximum number of bytes that can be sent in a message to the backend
	publishMetricRequestSizeLimit = 1024 * 1024
)

// tcsClientServer implements wsclient.ClientServer interface for metrics backend.
type tcsClientServer struct {
	doctor                   *doctor.Doctor
	pullInstanceStatusTicker *time.Ticker
	disableResourceMetrics   bool
	publishMetricsInterval   time.Duration

	metrics <-chan ecstcs.TelemetryMessage
	health  <-chan ecstcs.HealthMessage
	wsclient.ClientServerImpl
}

// New returns a client/server to bidirectionally communicate with the backend.
// The returned struct should have both 'Connect' and 'Serve' called upon it
// before being used.
func New(url string,
	cfg *wsclient.WSClientMinAgentConfig,
	doctor *doctor.Doctor,
	disableResourceMetrics bool,
	publishMetricsInterval time.Duration,
	credentialsCache *aws.CredentialsCache,
	rwTimeout time.Duration,
	metricsMessages <-chan ecstcs.TelemetryMessage,
	healthMessages <-chan ecstcs.HealthMessage,
	metricsFactory metrics.EntryFactory,
) wsclient.ClientServer {
	cs := &tcsClientServer{
		doctor:                   doctor,
		pullInstanceStatusTicker: nil,
		publishMetricsInterval:   publishMetricsInterval,
		metrics:                  metricsMessages,
		health:                   healthMessages,
		disableResourceMetrics:   disableResourceMetrics,
		ClientServerImpl: wsclient.ClientServerImpl{
			URL:              url,
			Cfg:              cfg,
			CredentialsCache: credentialsCache,
			RWTimeout:        rwTimeout,
			MakeRequestHook:  signRequestFunc(url, cfg.AWSRegion, credentialsCache),
			TypeDecoder:      NewTCSDecoder(),
			RequestHandlers:  make(map[string]wsclient.RequestHandler),
			MetricsFactory:   metricsFactory,
		},
	}
	cs.ServiceError = &tcsError{}
	return cs
}

// Serve begins serving requests using previously registered handlers (see
// AddRequestHandler). All request handlers should be added prior to making this
// call as unhandled requests will be discarded.
func (cs *tcsClientServer) Serve(ctx context.Context) error {
	logger.Debug("TCS client starting websocket poll loop")
	if !cs.IsReady() {
		return fmt.Errorf("tcs client: websocket not ready for connections")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Start the timer function to publish metrics to the backend.
	go cs.publishMessages(ctx)

	if cs.doctor != nil {
		cs.pullInstanceStatusTicker = time.NewTicker(cs.publishMetricsInterval)
		go cs.publishInstanceStatus(ctx)
	}

	return cs.ConsumeMessages(ctx)
}

func (cs *tcsClientServer) publishMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case metric := <-cs.metrics:
			logger.Debug("received telemetry message in metricsChannel")
			err := cs.publishMetricsOnce(metric)
			if err != nil {
				logger.Warn("Error publishing metrics", logger.Fields{
					field.Error: err,
				})
			}
		case health := <-cs.health:
			logger.Debug("received health message in healthChannel")
			err := cs.publishHealthOnce(health)
			if err != nil {
				logger.Warn("Error publishing health", logger.Fields{
					field.Error: err,
				})
			}
		}
	}
}

// publishMetricsOnce is invoked by the ticker to periodically publish metrics to backend.
func (cs *tcsClientServer) publishMetricsOnce(message ecstcs.TelemetryMessage) error {
	// Get the list of objects to send to backend.
	requests, err := cs.metricsToPublishMetricRequests(message)
	if err != nil {
		return err
	}

	// Make the publish metrics request to the backend.
	for _, request := range requests {
		logger.Debug("making publish metrics request")
		err = cs.MakeRequest(request)
		if err != nil {
			return err
		}
	}
	return nil
}

// publishHealthOnce is invoked by the ticker to periodically publish metrics to backend.
func (cs *tcsClientServer) publishHealthOnce(health ecstcs.HealthMessage) error {
	// Get the list of health request to send to backend.
	requests, err := cs.healthToPublishHealthRequests(health)
	if err != nil {
		return err
	}
	// Make the publish metrics request to the backend.
	for _, request := range requests {
		logger.Debug("making publish health metrics request")
		err = cs.MakeRequest(request)
		if err != nil {
			return err
		}
	}
	return nil
}

// metricsToPublishMetricRequests gets task metrics and converts them to a list of PublishMetricRequest
// objects.
func (cs *tcsClientServer) metricsToPublishMetricRequests(metrics ecstcs.TelemetryMessage) ([]*ecstcs.PublishMetricsRequest, error) {
	instanceMetrics, metadata, taskMetrics := metrics.InstanceMetrics, metrics.Metadata, metrics.TaskMetrics

	var requests []*ecstcs.PublishMetricsRequest
	if metadata == nil {
		return nil, seelog.Errorf("nil metrics metadata")
	}
	if *metadata.Idle {
		metadata.Fin = aws.Bool(true)
		// Idle instance, we have only one request to send to backend.
		requests = append(requests, ecstcs.NewPublishMetricsRequest(instanceMetrics, metadata, taskMetrics))
		return requests, nil
	}
	var messageInstanceMetrics *ecstcs.InstanceMetrics
	var messageTaskMetrics []*ecstcs.TaskMetric
	var requestMetadata *ecstcs.MetricsMetadata
	numTasks := len(taskMetrics)

	for i, taskMetric := range taskMetrics {
		// TACS expects that the instance metrics are included only in the first request.
		messageInstanceMetrics = filterInstanceMetrics(instanceMetrics, len(requests))
		requestMetadata = copyMetricsMetadata(metadata, false)

		// Check if taskMetric without service connect metrics exceed the message size
		tempTaskMetric := *taskMetric
		tempTaskMetric.ServiceConnectMetricsWrapper = tempTaskMetric.ServiceConnectMetricsWrapper[:0]

		messageTaskMetrics = append(messageTaskMetrics, &tempTaskMetric)
		tmsg, _ := jsonutil.BuildJSON(ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, copyTaskMetrics(messageTaskMetrics)))
		// remove the tempTaskMetric added to messageTaskMetrics after creating tempMessage
		messageTaskMetrics = messageTaskMetrics[:len(messageTaskMetrics)-1]
		if len(tmsg) > publishMetricRequestSizeLimit {
			// Create a new request as the current task metric if added is exceeding the size of the frame.
			requests = append(requests, ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, copyTaskMetrics(messageTaskMetrics)))
			// reset the messageTaskMetrics for the new request
			messageTaskMetrics = messageTaskMetrics[:0]
		}

		if taskMetric.ServiceConnectMetricsWrapper != nil {
			taskMetric, messageTaskMetrics, requests = cs.serviceConnectMetricsToPublishMetricRequests(messageInstanceMetrics, requestMetadata, taskMetric, messageTaskMetrics, requests)
		}
		messageTaskMetrics = append(messageTaskMetrics, taskMetric)
		if (i + 1) == numTasks {
			// If this is the last task to send, set fin to true
			requestMetadata = copyMetricsMetadata(metadata, true)
		}
		if len(messageTaskMetrics)%tasksInMetricMessage == 0 {
			// Construct payload with tasksInMetricMessage number of task metrics and send to backend.
			requests = append(requests, ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, copyTaskMetrics(messageTaskMetrics)))
			messageTaskMetrics = messageTaskMetrics[:0]
		}
	}

	if len(messageTaskMetrics) > 0 {
		messageInstanceMetrics = filterInstanceMetrics(instanceMetrics, len(requests))
		// Create the new metadata object and set fin to true as this is the last message in the payload.
		requestMetadata := copyMetricsMetadata(metadata, true)
		// Create a request with remaining task metrics.
		requests = append(requests, ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, messageTaskMetrics))
	}
	return requests, nil
}

// serviceConnectMetricsToPublishMetricRequests loops over all the SC metrics in a
// task metric to add SC metrics until the message size is within 1 MB.
// If adding a SC metric to the message exceeds the 1 MB limit, it will be sent in the new message
func (cs *tcsClientServer) serviceConnectMetricsToPublishMetricRequests(instanceMetrics *ecstcs.InstanceMetrics,
	requestMetadata *ecstcs.MetricsMetadata,
	taskMetric *ecstcs.TaskMetric,
	messageTaskMetrics []*ecstcs.TaskMetric,
	requests []*ecstcs.PublishMetricsRequest,
) (*ecstcs.TaskMetric, []*ecstcs.TaskMetric, []*ecstcs.PublishMetricsRequest) {
	var messageInstanceMetrics *ecstcs.InstanceMetrics
	tempTaskMetric := *taskMetric
	tempTaskMetric.ServiceConnectMetricsWrapper = tempTaskMetric.ServiceConnectMetricsWrapper[:0]

	for _, serviceConnectMetric := range taskMetric.ServiceConnectMetricsWrapper {
		messageInstanceMetrics = filterInstanceMetrics(instanceMetrics, len(requests))
		tempTaskMetric.ServiceConnectMetricsWrapper = append(tempTaskMetric.ServiceConnectMetricsWrapper, serviceConnectMetric)
		messageTaskMetrics = append(messageTaskMetrics, &tempTaskMetric)
		// TODO [SC]: Load test and profile this since BuildJSON results in lot of CPU and memory consumption.
		tempMessage, _ := jsonutil.BuildJSON(ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, copyTaskMetrics(messageTaskMetrics)))
		// remove the tempTaskMetric added to messageTaskMetrics after creating tempMessage
		messageTaskMetrics = messageTaskMetrics[:len(messageTaskMetrics)-1]
		if len(tempMessage) > publishMetricRequestSizeLimit {
			// since adding this SC metric to the message exceeds the 1 MB limit, remove it from taskMetric and create a request to send it to the backend
			tempTaskMetric.ServiceConnectMetricsWrapper = tempTaskMetric.ServiceConnectMetricsWrapper[:len(tempTaskMetric.ServiceConnectMetricsWrapper)-1]
			taskMetricTruncated := tempTaskMetric
			taskMetricTruncated.ServiceConnectMetricsWrapper = copyServiceConnectMetrics(tempTaskMetric.ServiceConnectMetricsWrapper)

			messageTaskMetrics = append(messageTaskMetrics, &taskMetricTruncated)
			requests = append(requests, ecstcs.NewPublishMetricsRequest(messageInstanceMetrics, requestMetadata, copyTaskMetrics(messageTaskMetrics)))

			// reset the messageTaskMetrics and tempTaskMetric for the new request,
			messageTaskMetrics = messageTaskMetrics[:0]
			tempTaskMetric.ServiceConnectMetricsWrapper = tempTaskMetric.ServiceConnectMetricsWrapper[:0]
			// container metrics will be sent only once for each task metric
			tempTaskMetric.ContainerMetrics = tempTaskMetric.ContainerMetrics[:0]
			// add the serviceConnectMetric to tempTaskMetric to be sent in the next message
			tempTaskMetric.ServiceConnectMetricsWrapper = append(tempTaskMetric.ServiceConnectMetricsWrapper, serviceConnectMetric)
		}
	}
	return &tempTaskMetric, messageTaskMetrics, requests
}

// healthToPublishHealthRequests creates the requests to publish container health
func (cs *tcsClientServer) healthToPublishHealthRequests(health ecstcs.HealthMessage) ([]*ecstcs.PublishHealthRequest, error) {
	metadata, taskHealthMetrics := health.Metadata, health.HealthMetrics

	if metadata == nil || taskHealthMetrics == nil {
		logger.Debug("No container health metrics to report")
		return nil, nil
	}

	var requests []*ecstcs.PublishHealthRequest
	var taskHealths []*ecstcs.TaskHealth
	numOfTasks := len(taskHealthMetrics)
	for i, taskHealth := range taskHealthMetrics {
		taskHealths = append(taskHealths, taskHealth)
		// create a request if the number of task reaches the maximum page size
		if (i+1)%tasksInHealthMessage == 0 {
			requestMetadata := copyHealthMetadata(metadata, (i+1) == numOfTasks)
			requestTaskHealth := copyTaskHealthMetrics(taskHealths)
			request := ecstcs.NewPublishHealthMetricsRequest(requestMetadata, requestTaskHealth)
			requests = append(requests, request)
			taskHealths = taskHealths[:0]
		}
	}

	// Put the rest of the metrics in another request
	if len(taskHealths) != 0 {
		requestMetadata := copyHealthMetadata(metadata, true)
		requests = append(requests, ecstcs.NewPublishHealthMetricsRequest(requestMetadata, taskHealths))
	}

	return requests, nil
}

// shouldIncludeInstanceMetrics determines if we want to include instance metrics in the telemetry request.
// Include instance metrics only in the first request.
func filterInstanceMetrics(instanceMetrics *ecstcs.InstanceMetrics, requestCount int) *ecstcs.InstanceMetrics {
	if instanceMetrics != nil && requestCount == 0 {
		return instanceMetrics
	}
	return nil
}

// copyMetricsMetadata creates a new MetricsMetadata object from a given MetricsMetadata object.
// It copies all the fields from the source object to the new object and sets the 'Fin' field
// as specified by the argument.
func copyMetricsMetadata(metadata *ecstcs.MetricsMetadata, fin bool) *ecstcs.MetricsMetadata {
	return &ecstcs.MetricsMetadata{
		Cluster:           aws.String(*metadata.Cluster),
		ContainerInstance: aws.String(*metadata.ContainerInstance),
		Idle:              aws.Bool(*metadata.Idle),
		MessageId:         aws.String(*metadata.MessageId),
		Fin:               aws.Bool(fin),
	}
}

// copyTaskMetrics copies a slice of TaskMetric objects to another slice. This is needed as we
// reset the source slice after creating a new PublishMetricsRequest object.
func copyTaskMetrics(from []*ecstcs.TaskMetric) []*ecstcs.TaskMetric {
	to := make([]*ecstcs.TaskMetric, len(from))
	copy(to, from)
	return to
}

// copyServiceConnectMetrics loops over list of GeneralMetricsWrapper obejcts and creates a new GeneralMetricsWrapper list
// and creates a new GeneralMetricsWrapper object from each given GeneralMetricsWrapper object.
func copyServiceConnectMetrics(scMetrics []*ecstcs.GeneralMetricsWrapper) []*ecstcs.GeneralMetricsWrapper {
	scMetricsTo := make([]*ecstcs.GeneralMetricsWrapper, len(scMetrics))
	for i, scMetricFrom := range scMetrics {
		scMetricTo := *scMetricFrom
		scMetricsTo[i] = &scMetricTo
	}
	return scMetricsTo
}

// copyHealthMetadata performs a deep copy of HealthMetadata object
func copyHealthMetadata(metadata *ecstcs.HealthMetadata, fin bool) *ecstcs.HealthMetadata {
	return &ecstcs.HealthMetadata{
		Cluster:           aws.String(aws.ToString(metadata.Cluster)),
		ContainerInstance: aws.String(aws.ToString(metadata.ContainerInstance)),
		Fin:               aws.Bool(fin),
		MessageId:         aws.String(aws.ToString(metadata.MessageId)),
	}
}

// copyTaskHealthMetrics copies a slice of taskHealthMetrics to another slice
func copyTaskHealthMetrics(from []*ecstcs.TaskHealth) []*ecstcs.TaskHealth {
	to := make([]*ecstcs.TaskHealth, len(from))
	copy(to, from)
	return to
}

// publishInstanceStatus queries the doctor.Doctor instance contained within cs,
// converts the healthcheck results to an InstanceStatusRequest and then sends it
// to the backend
func (cs *tcsClientServer) publishInstanceStatus(ctx context.Context) {
	// Note to disambiguate between health metrics and instance statuses
	//
	// Instance status checks are performed by the Doctor class in the doctor module
	// but the code calls them Healthchecks. They are named such because they denote
	// that the container runtime (Docker) is healthy and communicating with the
	// ECS Agent. Container instance statuses, which this function handles,
	// pertain to the status of this container instance.
	//
	// Health metrics are specific to the tasks that are running on this particular
	// container instance. Health metrics, which the publishHealthMetrics function
	// handles, pertain to the health of the tasks that are running on this
	// container instance.
	if cs.pullInstanceStatusTicker == nil {
		logger.Debug("Skipping publishing container instance statuses. Publish ticker is uninitialized")
		return
	}

	for {
		select {
		case <-cs.pullInstanceStatusTicker.C:
			if !cs.doctor.HasStatusBeenReported() {
				err := cs.publishInstanceStatusOnce()
				if err != nil {
					logger.Warn("Unable to publish instance status", logger.Fields{
						field.Error: err,
					})
				} else {
					cs.doctor.SetStatusReported(true)
				}
			} else {
				logger.Debug("Skipping publishing container instance status message that was already sent")
			}
		case <-ctx.Done():
			return
		}
	}
}

// publishInstanceStatusOnce gets called on a ticker to pull instance status
// from the doctor instance contained within cs and sned that information to
// the backend
func (cs *tcsClientServer) publishInstanceStatusOnce() error {
	// Get the list of health request to send to backend.
	request, err := cs.getPublishInstanceStatusRequest()
	if err != nil {
		return err
	}

	// Make the publish instance status request to the backend.
	err = cs.MakeRequest(request)
	if err != nil {
		return err
	}

	cs.doctor.SetStatusReported(true)

	return nil
}

// GetPublishInstanceStatusRequest will get all healthcheck statuses and generate
// a sendable PublishInstanceStatusRequest
func (cs *tcsClientServer) getPublishInstanceStatusRequest() (*ecstcs.PublishInstanceStatusRequest, error) {
	metadata := &ecstcs.InstanceStatusMetadata{
		Cluster:           aws.String(cs.doctor.GetCluster()),
		ContainerInstance: aws.String(cs.doctor.GetContainerInstanceArn()),
		RequestId:         aws.String(uuid.NewRandom().String()),
	}
	instanceStatuses := cs.getInstanceStatuses()
	if instanceStatuses == nil {
		return nil, doctor.EmptyHealthcheckError
	}

	return &ecstcs.PublishInstanceStatusRequest{
		Metadata:  metadata,
		Statuses:  instanceStatuses,
		Timestamp: aws.Time(time.Now()),
	}, nil
}

// getInstanceStatuses returns a list of instance statuses converted from what
// the doctor knows about the registered healthchecks
func (cs *tcsClientServer) getInstanceStatuses() []*ecstcs.InstanceStatus {
	var instanceStatuses []*ecstcs.InstanceStatus

	for _, healthcheck := range *cs.doctor.GetHealthchecks() {
		instanceStatus := &ecstcs.InstanceStatus{
			LastStatusChange: aws.Time(healthcheck.GetStatusChangeTime()),
			LastUpdated:      aws.Time(healthcheck.GetLastHealthcheckTime()),
			Status:           aws.String(healthcheck.GetHealthcheckStatus().String()),
			Type:             aws.String(healthcheck.GetHealthcheckType()),
		}
		instanceStatuses = append(instanceStatuses, instanceStatus)
	}
	return instanceStatuses
}

// Close closes the underlying connection.
func (cs *tcsClientServer) Close() error {
	if cs.pullInstanceStatusTicker != nil {
		cs.pullInstanceStatusTicker.Stop()
	}

	return cs.Disconnect()
}

// signRequestFunc is a MakeRequestHookFunc that signs each generated request
func signRequestFunc(url, region string, credentialsCache *aws.CredentialsCache) wsclient.MakeRequestHookFunc {
	return func(payload []byte) ([]byte, error) {
		reqBody := bytes.NewReader(payload)

		request, err := http.NewRequest("GET", url, reqBody)
		if err != nil {
			return nil, err
		}

		// hack to get v2 creds into v1 object.
		// TODO: Can be removed once TCS adds support for AWS SDK v2 Credentials
		credentialsProvider, err := credentialsCache.Retrieve(context.TODO())
		if err != nil || !credentialsProvider.HasKeys() {
			logger.Error("Error getting valid credentials", logger.Fields{
				field.Error: err,
			})
			return nil, err
		}
		creds := awscreds.NewStaticCredentials(credentialsProvider.AccessKeyID, credentialsProvider.SecretAccessKey, credentialsProvider.SessionToken)

		// TODO: Modify this to use SignHTTPRequest() when TCS adds support for AWS SDK v2 Credentials
		err = utils.SignHTTPRequestV1(request, region, "ecs", creds, reqBody)
		if err != nil {
			return nil, err
		}

		request.Header.Add("Host", request.Host)
		var dataBuffer bytes.Buffer
		request.Header.Write(&dataBuffer)
		io.WriteString(&dataBuffer, "\r\n")

		data := dataBuffer.Bytes()
		data = append(data, payload...)

		return data, nil
	}
}
