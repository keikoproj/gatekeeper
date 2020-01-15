package service

import (
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/keikoproj/lifecycle-manager/pkg/log"
	"github.com/keikoproj/lifecycle-manager/pkg/version"
	"github.com/pkg/errors"
)

var (
	// TerminationEventName is the event name of a terminating lifecycle hook
	TerminationEventName = "autoscaling:EC2_INSTANCE_TERMINATING"
	// ContinueAction is the name of the action in case we are successful in draining
	ContinueAction = "CONTINUE"
	// AbandonAction is the name of the action in case we are unsuccessful in draining
	AbandonAction = "ABANDON"
	// ExcludeLabelKey is the alb-ingress-controller exclude label key
	ExcludeLabelKey = "alpha.service-controller.kubernetes.io/exclude-balancer"
	// ExcludeLabelValue is the alb-ingress-controller exclude label value
	ExcludeLabelValue = "true"
	// InProgressAnnotationKey is the annotation key for setting the state of a node to in-progress
	InProgressAnnotationKey = "lifecycle-manager.keikoproj.io/in-progress"
	// ThreadJitterRangeSeconds configures the jitter range in seconds 0 to N per handler goroutine
	ThreadJitterRangeSeconds = 30.0
	// IterationJitterRangeSeconds configures the jitter range in seconds 0 to N per call iteration goroutine
	IterationJitterRangeSeconds = 1.0
	// NodeAgeCacheTTL defines a node age in minutes for which all caches are flushed
	NodeAgeCacheTTL = 90
	// WaiterDelayIntervalSeconds defines the default polling interval for waiters
	WaiterDelayIntervalSeconds int64 = 30
	// WaiterMaxAttempts defines the maximum attempts a waiter will make before timing out
	WaiterMaxAttempts = 500
)

// Start starts the lifecycle-manager service
func (mgr *Manager) Start() {
	var (
		ctx     = &mgr.context
		metrics = mgr.metrics
		kube    = mgr.authenticator.KubernetesClient
	)

	log.Infof("starting lifecycle-manager service v%v", version.Version)
	log.Infof("region = %v", ctx.Region)
	log.Infof("queue = %v", ctx.QueueName)
	log.Infof("polling interval seconds = %v", ctx.PollingIntervalSeconds)
	log.Infof("node drain timeout seconds = %v", ctx.DrainTimeoutSeconds)
	log.Infof("node drain retry interval seconds = %v", ctx.DrainRetryIntervalSeconds)
	log.Infof("with alb deregister = %v", ctx.WithDeregister)

	// start metrics server
	log.Infof("starting metrics server on %v%v", MetricsEndpoint, MetricsPort)
	go metrics.Start()

	// create a poller goroutine that reads from sqs and posts to channel
	log.Info("spawning sqs poller")
	go mgr.newPoller()

	// restore in-progress events if crashed
	inProgressEvents, err := getNodesByAnnotationKey(kube, InProgressAnnotationKey)
	if err != nil {
		log.Errorf("failed to resume in progress events: %v", err)
	}

	for node, sqsMessage := range inProgressEvents {
		if sqsMessage == "" {
			continue
		}
		log.Infof("trying to resume termination of node/%v", node)
		message, err := deserializeMessage(sqsMessage)
		if err != nil {
			log.Errorf("failed to resume in progress events: %v", err)
		}
		go mgr.newWorker(message)
	}

	// process messags from channel
	for message := range mgr.eventStream {
		go mgr.newWorker(message)
	}
}

// Process processes a received event
func (mgr *Manager) Process(event *LifecycleEvent) error {

	// add event to work queue
	mgr.AddEvent(event)

	log.Infof("received termination event for instance/%v", event.EC2InstanceID)

	// handle event
	err := mgr.handleEvent(event)
	if err != nil {
		return err
	}

	// mark event as completed
	mgr.CompleteEvent(event)

	return nil
}

func (mgr *Manager) AddEvent(event *LifecycleEvent) {
	var (
		metrics   = mgr.metrics
		queueSync = mgr.workQueueSync
	)
	queueSync.Lock()
	event.SetEventTimeStarted(time.Now())
	metrics.IncGauge(TerminatingInstancesCountMetric)
	mgr.workQueue = append(mgr.workQueue, event)
	queueSync.Unlock()
}

func (mgr *Manager) CompleteEvent(event *LifecycleEvent) {
	var (
		queue      = mgr.authenticator.SQSClient
		metrics    = mgr.metrics
		kubeClient = mgr.authenticator.KubernetesClient
		asgClient  = mgr.authenticator.ScalingGroupClient
		url        = event.queueURL
		t          = time.Since(event.startTime).Seconds()
	)

	if mgr.avarageLatency == 0 {
		mgr.avarageLatency = t
	} else {
		mgr.avarageLatency = (mgr.avarageLatency + t) / 2
	}

	newQueue := make([]*LifecycleEvent, 0)
	for _, e := range mgr.workQueue {
		if reflect.DeepEqual(event, e) {
			// found event in work queue
			log.Infof("event %v completed processing", event.RequestID)
			event.SetEventCompleted(true)

			err := deleteMessage(queue, url, event.receiptHandle)
			if err != nil {
				log.Errorf("failed to delete message: %v", err)
			}
			err = completeLifecycleAction(asgClient, *event, ContinueAction)
			if err != nil {
				log.Errorf("failed to complete lifecycle action: %v", err)
			}
			msg := fmt.Sprintf(EventMessageLifecycleHookProcessed, event.RequestID, event.EC2InstanceID, t)
			msgFields := map[string]string{
				"eventID":       event.RequestID,
				"ec2InstanceId": event.EC2InstanceID,
				"asgName":       event.AutoScalingGroupName,
				"details":       msg,
			}
			publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonLifecycleHookProcessed, msgFields, event.referencedNode.Name))
			metrics.AddCounter(SuccessfulEventsTotalMetric, 1)
		} else {
			newQueue = append(newQueue, e)
		}
	}
	mgr.workQueueSync.Lock()
	mgr.workQueue = newQueue
	mgr.completedEvents++
	metrics.DecGauge(TerminatingInstancesCountMetric)
	metrics.SetGauge(AverageDurationSecondsMetric, mgr.avarageLatency)
	log.Infof("event %v for instance %v completed after %vs", event.RequestID, event.EC2InstanceID, t)
	mgr.workQueueSync.Unlock()
}

func (mgr *Manager) FailEvent(err error, event *LifecycleEvent, abandon bool) {
	var (
		auth               = mgr.authenticator
		kubeClient         = auth.KubernetesClient
		queue              = auth.SQSClient
		metrics            = mgr.metrics
		scalingGroupClient = auth.ScalingGroupClient
		url                = event.queueURL
		t                  = time.Since(event.startTime).Seconds()
	)
	log.Errorf("event %v has failed processing after %vs: %v", event.RequestID, t, err)
	mgr.failedEvents++
	metrics.AddCounter(FailedEventsTotalMetric, 1)
	event.SetEventCompleted(true)
	msg := fmt.Sprintf(EventMessageLifecycleHookFailed, event.RequestID, t, err)
	msgFields := map[string]string{
		"eventID":       event.RequestID,
		"ec2InstanceId": event.EC2InstanceID,
		"asgName":       event.AutoScalingGroupName,
		"details":       msg,
	}
	publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonLifecycleHookFailed, msgFields, event.referencedNode.Name))

	if abandon {
		log.Warnf("abandoning instance %v", event.EC2InstanceID)
		err := completeLifecycleAction(scalingGroupClient, *event, AbandonAction)
		if err != nil {
			log.Errorf("completeLifecycleAction Failed, %s", err)
		}
	}

	if reflect.DeepEqual(event, LifecycleEvent{}) {
		log.Errorf("event failed: invalid message: %v", err)
		return
	}

	err = deleteMessage(queue, url, event.receiptHandle)
	if err != nil {
		log.Errorf("event failed: failed to delete message: %v", err)
	}

}

func (mgr *Manager) RejectEvent(err error, event *LifecycleEvent) {
	var (
		metrics = mgr.metrics
		auth    = mgr.authenticator
		queue   = auth.SQSClient
		url     = event.queueURL
	)

	log.Debugf("event %v has been rejected for processing: %v", event.RequestID, err)
	mgr.rejectedEvents++
	metrics.AddCounter(RejectedEventsTotalMetric, 1)

	if reflect.DeepEqual(event, LifecycleEvent{}) {
		log.Errorf("event failed: invalid message: %v", err)
		return
	}

	err = deleteMessage(queue, url, event.receiptHandle)
	if err != nil {
		log.Errorf("failed to delete message: %v", err)
	}
}

func (mgr *Manager) newPoller() {
	var (
		ctx      = &mgr.context
		metrics  = mgr.metrics
		auth     = mgr.authenticator
		stream   = mgr.eventStream
		queue    = auth.SQSClient
		url      = getQueueURLByName(queue, ctx.QueueName)
		interval = ctx.PollingIntervalSeconds
	)

	for {
		log.Debugln("polling for messages from queue")
		goroutines := runtime.NumGoroutine()
		metrics.SetGauge(ActiveGoroutinesMetric, float64(goroutines))
		log.Debugf("active goroutines: %v", goroutines)

		output, err := queue.ReceiveMessage(&sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url),
			AttributeNames: aws.StringSlice([]string{
				"SenderId",
			}),
			MaxNumberOfMessages: aws.Int64(1),
			WaitTimeSeconds:     aws.Int64(interval),
		})
		if err != nil {
			log.Errorf("unable to receive message from queue %s, %v.", url, err)
			time.Sleep(time.Duration(interval) * time.Second)
		}
		if len(output.Messages) == 0 {
			log.Debugln("no messages received in interval")
		}
		for _, message := range output.Messages {
			stream <- message
		}
	}
}

func (mgr *Manager) newWorker(message *sqs.Message) {
	var (
		auth       = mgr.authenticator
		kubeClient = auth.KubernetesClient
		queue      = auth.SQSClient
		ctx        = &mgr.context
		url        = getQueueURLByName(queue, ctx.QueueName)
	)

	// process messags from channel
	event, err := readMessage(message)
	if err != nil {
		err = errors.Wrap(err, "failed to read message")
		mgr.RejectEvent(err, event)
		return
	}
	event.SetQueueURL(url)

	if !event.IsValid() {
		err = errors.Wrap(err, "received invalid event")
		mgr.RejectEvent(err, event)
		return
	}

	if event.IsAlreadyExist(mgr.workQueue) {
		err := errors.New("event already exists")
		mgr.RejectEvent(err, event)
		return
	}

	heartbeatInterval, err := getHookHeartbeatInterval(auth.ScalingGroupClient, event.LifecycleHookName, event.AutoScalingGroupName)
	if err != nil {
		err = errors.Wrap(err, "failed to get hook heartbeat interval")
		mgr.RejectEvent(err, event)
		return
	}
	event.SetHeartbeatInterval(heartbeatInterval)

	node, exists := getNodeByInstance(kubeClient, event.EC2InstanceID)
	if !exists {
		err = errors.Errorf("instance %v is not seen in cluster nodes", event.EC2InstanceID)
		mgr.RejectEvent(err, event)
		return
	}
	event.SetReferencedNode(node)
	event.SetMessage(message)

	msg := fmt.Sprintf(EventMessageLifecycleHookReceived, event.RequestID, event.EC2InstanceID)
	msgFields := map[string]string{
		"eventID":       event.RequestID,
		"ec2InstanceId": event.EC2InstanceID,
		"asgName":       event.AutoScalingGroupName,
		"details":       msg,
	}
	publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonLifecycleHookReceived, msgFields, event.referencedNode.Name))

	err = mgr.Process(event)
	if err != nil {
		mgr.FailEvent(err, event, true)
		return
	}
}

func (mgr *Manager) drainNodeTarget(event *LifecycleEvent) error {
	var (
		ctx           = &mgr.context
		kubeClient    = mgr.authenticator.KubernetesClient
		kubectlPath   = mgr.context.KubectlLocalPath
		metrics       = mgr.metrics
		drainTimeout  = ctx.DrainTimeoutSeconds
		retryInterval = ctx.DrainRetryIntervalSeconds
		successMsg    = fmt.Sprintf(EventMessageNodeDrainSucceeded, event.referencedNode.Name)
	)

	err := drainNode(kubectlPath, event.referencedNode.Name, drainTimeout, retryInterval)
	if err != nil {
		failMsg := fmt.Sprintf(EventMessageNodeDrainFailed, event.referencedNode.Name, err)
		msgFields := map[string]string{
			"eventID":       event.RequestID,
			"ec2InstanceId": event.EC2InstanceID,
			"asgName":       event.AutoScalingGroupName,
			"details":       failMsg,
		}
		publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonNodeDrainFailed, msgFields, event.referencedNode.Name))
		return err
	}
	log.Infof("completed drain for node %v", event.referencedNode.Name)
	event.SetDrainCompleted(true)
	metrics.AddCounter(SuccessfulNodeDrainTotalMetric, 1)

	msgFields := map[string]string{
		"eventID":       event.RequestID,
		"ec2InstanceId": event.EC2InstanceID,
		"asgName":       event.AutoScalingGroupName,
		"details":       successMsg,
	}
	publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonNodeDrainSucceeded, msgFields, event.referencedNode.Name))
	return nil
}

func (mgr *Manager) drainLoadbalancerTarget(event *LifecycleEvent) error {
	var (
		kubeClient          = mgr.authenticator.KubernetesClient
		elbv2Client         = mgr.authenticator.ELBv2Client
		elbClient           = mgr.authenticator.ELBClient
		instanceID          = event.EC2InstanceID
		ctx                 = &mgr.context
		metrics             = mgr.metrics
		node                = event.referencedNode
		wg                  sync.WaitGroup
		finished            = make(chan bool, 1)
		activeTargetGroups  = make(map[string]int64)
		activeLoadBalancers = make([]string, 0)
	)

	if !ctx.WithDeregister {
		return nil
	}

	// sleep for random jitter per goroutine
	waitJitter(ThreadJitterRangeSeconds)

	// add exclusion label
	log.Debugf("excluding node %v from load balancers", node.Name)
	err := labelNode(ctx.KubectlLocalPath, node.Name, ExcludeLabelKey, ExcludeLabelValue)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	nodeCreationTime := node.CreationTimestamp.UTC()
	nodeAge := int(now.Sub(nodeCreationTime).Minutes())
	if nodeAge <= NodeAgeCacheTTL {
		log.Infof("Node younger than %vm was terminated, flushing DescribeTargetHealth caches", NodeAgeCacheTTL)
		mgr.context.CacheConfig.FlushCache("elasticloadbalancing.DescribeTargetHealth")
		mgr.context.CacheConfig.FlushCache("elasticloadbalancing.DescribeInstanceHealth")
	}

	// get all target groups
	targetGroups := []*elbv2.TargetGroup{}
	err = elbv2Client.DescribeTargetGroupsPages(&elbv2.DescribeTargetGroupsInput{}, func(page *elbv2.DescribeTargetGroupsOutput, lastPage bool) bool {
		targetGroups = append(targetGroups, page.TargetGroups...)
		return page.NextMarker != nil
	})
	if err != nil {
		return err
	}

	// get all classic elbs
	elbDescriptions := []*elb.LoadBalancerDescription{}
	err = elbClient.DescribeLoadBalancersPages(&elb.DescribeLoadBalancersInput{}, func(page *elb.DescribeLoadBalancersOutput, lastPage bool) bool {
		elbDescriptions = append(elbDescriptions, page.LoadBalancerDescriptions...)
		return page.NextMarker != nil
	})
	if err != nil {
		return err
	}

	log.Infof("checking targetgroup/elb membership for %v", instanceID)
	// find instance in target groups
	for i, tg := range targetGroups {
		arn := aws.StringValue(tg.TargetGroupArn)
		// check each target group for matches
		waitJitter(IterationJitterRangeSeconds)
		log.Debugf("checking membership of %v on %v (%v/%v)", instanceID, arn, i, len(targetGroups))
		found, port, err := findInstanceInTargetGroup(elbv2Client, arn, instanceID)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == elbv2.ErrCodeTargetGroupNotFoundException {
					log.Warnf("target group %v not found, skipping", arn)
					continue
				}
			}
			return err
		}

		if !found {
			continue
		}
		activeTargetGroups[arn] = port
	}

	// find instance in classic elbs
	for i, desc := range elbDescriptions {
		elbName := aws.StringValue(desc.LoadBalancerName)
		// check each target group for matches
		waitJitter(IterationJitterRangeSeconds)
		log.Debugf("checking membership of %v on %v (%v/%v)", instanceID, elbName, i, len(elbDescriptions))
		found, err := findInstanceInClassicBalancer(elbClient, elbName, instanceID)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == elb.ErrCodeAccessPointNotFoundException {
					log.Warnf("classic-elb %v not found, skipping", elbName)
					continue
				}
			}
			return err
		}

		if !found {
			continue
		}
		activeLoadBalancers = append(activeLoadBalancers, elbName)
	}

	// create goroutine per target group with target match
	workQueueLength := len(activeTargetGroups) + len(activeLoadBalancers)
	wg.Add(workQueueLength)
	errChannel := make(chan error, workQueueLength*2)

	log.Infof("found %v target groups & %v classic-elb for instance %v", len(activeTargetGroups), len(elbDescriptions), instanceID)

	log.Infof("starting deregistration for %v", instanceID)
	// handle classic load balancers deregistration
	deregisteredLoadBalancers := []string{}
	for i, elbName := range activeLoadBalancers {

		if event.eventCompleted {
			return errors.New("event finished execution during deregistration")
		}

		// sleep for random jitter per iteration
		waitJitter(IterationJitterRangeSeconds)
		log.Debugf("deregistering %v from %v (%v/%v)", instanceID, elbName, i+1, len(activeTargetGroups))
		err = deregisterInstance(elbClient, elbName, instanceID)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == elb.ErrCodeAccessPointNotFoundException {
					log.Warnf("ELB %v not found, skipping", elbName)
					continue
				} else if awsErr.Code() == elb.ErrCodeInvalidEndPointException {
					log.Warnf("ELB target %v not found in %v, skipping", instanceID, elbName)
					continue
				}
			}
			log.Errorf("instance %v deregistration failed: %v", instanceID, err)
			errChannel <- err
			msg := fmt.Sprintf(EventMessageInstanceDeregisterFailed, instanceID, elbName, err)
			msgFields := map[string]string{
				"elbName":       elbName,
				"ec2InstanceId": instanceID,
				"elbType":       "classic-elb",
				"details":       msg,
			}
			publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonInstanceDeregisterFailed, msgFields, event.referencedNode.Name))
			continue
		}
		deregisteredLoadBalancers = append(deregisteredLoadBalancers, elbName)
	}

	// handle v2 load balancers deregistration
	deregisteredTargetGroups := map[string]int64{}
	currentDeregistering := 0
	for arn, port := range activeTargetGroups {

		if event.eventCompleted {
			return errors.New("event finished execution during deregistration")
		}

		currentDeregistering++
		// sleep for random jitter per iteration
		waitJitter(IterationJitterRangeSeconds)
		log.Debugf("deregistering %v from %v (%v/%v)", instanceID, arn, currentDeregistering, len(activeTargetGroups))
		err = deregisterTarget(elbv2Client, arn, instanceID, port)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == elbv2.ErrCodeTargetGroupNotFoundException {
					log.Warnf("target group %v not found, skipping", arn)
					continue
				} else if awsErr.Code() == elbv2.ErrCodeInvalidTargetException {
					log.Warnf("target %v not found in target group %v, skipping", instanceID, arn)
					continue
				}
			}
			log.Errorf("target %v deregistration failed: %v", instanceID, err)
			errChannel <- err
			msg := fmt.Sprintf(EventMessageTargetDeregisterFailed, instanceID, port, arn, err)
			msgFields := map[string]string{
				"port":          fmt.Sprintf("%d", port),
				"targetGroup":   arn,
				"ec2InstanceId": instanceID,
				"elbType":       "alb",
				"details":       msg,
			}
			publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonTargetDeregisterFailed, msgFields, event.referencedNode.Name))
			continue
		}
		deregisteredTargetGroups[arn] = port
	}

	log.Infof("starting waiters for %v", instanceID)
	// spawn waiters for classic elb
	for _, elbName := range deregisteredLoadBalancers {
		// sleep for random jitter per waiter
		waitJitter(IterationJitterRangeSeconds)
		go func(elbName, instance string) {
			defer wg.Done()
			// wait for deregister/drain
			log.Debugf("starting elb-drain waiter for %v in classic-elb %v", instance, elbName)
			err = waitForDeregisterInstance(event, elbClient, elbName, instance)
			if err != nil {
				if awsErr, ok := err.(awserr.Error); ok {
					if awsErr.Code() == elb.ErrCodeAccessPointNotFoundException {
						log.Warnf("ELB %v not found, skipping", elbName)
						return
					}
				}
				errChannel <- err
				return
			}

			// publish event
			msg := fmt.Sprintf(EventMessageInstanceDeregisterSucceeded, instance, elbName)
			msgFields := map[string]string{
				"elbName":       elbName,
				"ec2InstanceId": instance,
				"elbType":       "classic-elb",
				"details":       msg,
			}
			publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonInstanceDeregisterSucceeded, msgFields, event.referencedNode.Name))
		}(elbName, instanceID)
	}

	// spawn waiters for target groups
	for arn, port := range deregisteredTargetGroups {
		// sleep for random jitter per waiter
		waitJitter(IterationJitterRangeSeconds)
		go func(activeARN, instance string, activePort int64) {
			defer wg.Done()
			// wait for deregister/drain
			log.Debugf("starting alb-drain waiter for %v in target-group %v", instance, activeARN)
			err = waitForDeregisterTarget(event, elbv2Client, activeARN, instance, activePort)
			if err != nil {
				if awsErr, ok := err.(awserr.Error); ok {
					if awsErr.Code() == elbv2.ErrCodeTargetGroupNotFoundException {
						log.Warnf("target group %v not found, skipping", arn)
						return
					}
				}
				errChannel <- err
				return
			}

			// publish event
			msg := fmt.Sprintf(EventMessageTargetDeregisterSucceeded, instance, activePort, activeARN)
			msgFields := map[string]string{
				"port":          fmt.Sprintf("%d", activePort),
				"targetGroup":   activeARN,
				"ec2InstanceId": instance,
				"elbType":       "alb",
				"details":       msg,
			}
			publishKubernetesEvent(kubeClient, newKubernetesEvent(EventReasonTargetDeregisterSucceeded, msgFields, event.referencedNode.Name))
		}(arn, instanceID, port)
	}

	// wait indefinitely for goroutines to complete
	go func() {
		wg.Wait()
		close(finished)
	}()

	var errs error
	select {
	case <-finished:
	case err := <-errChannel:
		if err != nil {
			errs = errors.Wrap(err, "failed to process alb-drain: ")
		}
	}

	if errs != nil {
		return errs
	}

	log.Debugf("successfully executed all drainLoadbalancerTarget goroutines")
	metrics.AddCounter(SuccessfulLBDeregisterTotalMetric, 1)
	event.SetDeregisterCompleted(true)
	return nil
}

func (mgr *Manager) handleEvent(event *LifecycleEvent) error {
	var (
		asgClient = mgr.authenticator.ScalingGroupClient
		metrics   = mgr.metrics
	)

	// send heartbeat at intervals
	go sendHeartbeat(asgClient, event)

	// Annotate node with InProgressAnnotationKey = EventBody for resuming in case of crash
	storeMessage, err := serializeMessage(event.message)
	if err != nil {
		log.Errorf("failed to serialize message for storage, event cannot be restored")
	} else {
		annotateNode(mgr.context.KubectlLocalPath, event.referencedNode.Name, InProgressAnnotationKey, string(storeMessage))
	}

	// node drain
	metrics.IncGauge(DrainingInstancesCountMetric)
	err = mgr.drainNodeTarget(event)
	if err != nil {
		metrics.DecGauge(DrainingInstancesCountMetric)
		metrics.AddCounter(FailedNodeDrainTotalMetric, 1)
	}
	metrics.DecGauge(DrainingInstancesCountMetric)

	// alb-drain action
	metrics.IncGauge(DeregisteringInstancesCountMetric)
	err = mgr.drainLoadbalancerTarget(event)
	if err != nil {
		metrics.DecGauge(DeregisteringInstancesCountMetric)
		metrics.AddCounter(FailedLBDeregisterTotalMetric, 1)
	}
	metrics.DecGauge(DeregisteringInstancesCountMetric)

	if err != nil {
		return err
	}

	// clear the state annotation once processing is ended
	annotateNode(mgr.context.KubectlLocalPath, event.referencedNode.Name, InProgressAnnotationKey, "")

	return nil
}

func waitJitter(max float64) {
	min := 0.3
	rand.Seed(time.Now().UnixNano())
	r := min + rand.Float64()*(max-min)
	log.Debugf("adding jitter of %v seconds to waiter\n", r)
	time.Sleep(time.Duration(r) * time.Second)
}
