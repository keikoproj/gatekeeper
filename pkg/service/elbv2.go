package service

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"

	"github.com/keikoproj/lifecycle-manager/pkg/log"
)

func waitForDeregisterTarget(elbClient elbv2iface.ELBV2API, arn, instanceID string, port int64) error {
	var (
		MaxAttempts = 500
	)

	waiterOpts := []request.WaiterOption{
		request.WithWaiterMaxAttempts(MaxAttempts),
	}

	input := &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(arn),
		Targets: []*elbv2.TargetDescription{
			{
				Id:   aws.String(instanceID),
				Port: aws.Int64(port),
			},
		},
	}

	err := elbClient.WaitUntilTargetDeregisteredWithContext(context.Background(), input, waiterOpts...)
	if err != nil {
		return err
	}
	return nil
}

func findInstanceInTargetGroup(elbClient elbv2iface.ELBV2API, arn, instanceID string) (bool, int64, error) {
	input := &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(arn),
	}

	target, err := elbClient.DescribeTargetHealth(input)
	if err != nil {
		log.Infof("failed finding instance %v in target group %v: %v", instanceID, arn, err.Error())
		return false, 0, err
	}
	for _, desc := range target.TargetHealthDescriptions {
		if aws.StringValue(desc.Target.Id) == instanceID {
			port := aws.Int64Value(desc.Target.Port)
			return true, port, nil
		}
	}
	return false, 0, nil
}

func deregisterTarget(elbClient elbv2iface.ELBV2API, arn, instanceID string, port int64) error {
	input := &elbv2.DeregisterTargetsInput{
		Targets: []*elbv2.TargetDescription{
			{
				Id:   aws.String(instanceID),
				Port: aws.Int64(port),
			},
		},
		TargetGroupArn: aws.String(arn),
	}

	log.Infof("deregistering %v from %v", instanceID, arn)
	_, err := elbClient.DeregisterTargets(input)
	if err != nil {
		return err
	}
	return nil
}
