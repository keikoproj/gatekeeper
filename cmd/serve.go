package cmd

import (
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws/request"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
	"github.com/keikoproj/lifecycle-manager/pkg/log"
	"github.com/keikoproj/lifecycle-manager/pkg/service"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	localMode        string
	region           string
	queueName        string
	kubectlLocalPath string
	nodeName         string
	logLevel         string

	deregisterTargetGroups bool

	drainRetryIntervalSeconds int
	drainTimeoutSeconds       int
	pollingIntervalSeconds    int

	// DefaultRetryer is the default retry configuration for some AWS API calls
	DefaultRetryer = client.DefaultRetryer{
		NumMaxRetries:    250,
		MinThrottleDelay: time.Second * 5,
		MaxThrottleDelay: time.Second * 60,
		MinRetryDelay:    time.Second * 1,
		MaxRetryDelay:    time.Second * 5,
	}
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "start the lifecycle-manager service",
	Long:  `Start watching lifecycle events for a given queue`,
	Run: func(cmd *cobra.Command, args []string) {
		// argument validation
		validate()
		log.SetLevel(logLevel)

		// prepare auth clients
		auth := service.Authenticator{
			ScalingGroupClient: newASGClient(region),
			SQSClient:          newSQSClient(region),
			ELBv2Client:        newELBv2Client(region),
			ELBClient:          newELBClient(region),
			KubernetesClient:   newKubernetesClient(localMode),
		}

		// prepare runtime context
		context := service.ManagerContext{
			KubectlLocalPath:          kubectlLocalPath,
			QueueName:                 queueName,
			DrainTimeoutSeconds:       int64(drainTimeoutSeconds),
			PollingIntervalSeconds:    int64(pollingIntervalSeconds),
			DrainRetryIntervalSeconds: int64(drainRetryIntervalSeconds),
			Region:                    region,
			WithDeregister:            deregisterTargetGroups,
		}

		s := service.New(auth, context)
		s.Start()
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().StringVar(&localMode, "local-mode", "", "absolute path to kubeconfig")
	serveCmd.Flags().StringVar(&region, "region", "", "AWS region to operate in")
	serveCmd.Flags().StringVar(&queueName, "queue-name", "", "the name of the SQS queue to consume lifecycle hooks from")
	serveCmd.Flags().StringVar(&kubectlLocalPath, "kubectl-path", "/usr/local/bin/kubectl", "the path to kubectl binary")
	serveCmd.Flags().StringVar(&logLevel, "log-level", "info", "the logging level (info, warning, debug)")
	serveCmd.Flags().IntVar(&drainTimeoutSeconds, "drain-timeout", 300, "hard time limit for drain")
	serveCmd.Flags().IntVar(&drainRetryIntervalSeconds, "drain-interval", 30, "interval in seconds for which to retry draining")
	serveCmd.Flags().IntVar(&pollingIntervalSeconds, "polling-interval", 10, "interval in seconds for which to poll SQS")
	serveCmd.Flags().BoolVar(&deregisterTargetGroups, "with-deregister", true, "try to deregister deleting instance from target groups")
}

func validate() {
	if localMode != "" {
		if _, err := os.Stat(localMode); os.IsNotExist(err) {
			log.Fatalf("provided kubeconfig path does not exist")
		}
	}

	if kubectlLocalPath != "" {
		if _, err := os.Stat(kubectlLocalPath); os.IsNotExist(err) {
			log.Fatalf("provided kubectl path does not exist")
		}
	} else {
		log.Fatalf("must provide kubectl path")
	}

	if region == "" {
		log.Fatalf("must provide valid AWS region name")
	}

	if queueName == "" {
		log.Fatalf("must provide valid SQS queue name")
	}
}

func newKubernetesClient(localMode string) *kubernetes.Clientset {
	var config *rest.Config
	var err error

	if localMode != "" {
		// use kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", localMode)
		if err != nil {
			log.Fatalf("cannot load kubernetes config from '%v'", localMode)
		}
	} else {
		// use InCluster auth
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalln("cannot load kubernetes config from InCluster")
		}
	}
	return kubernetes.NewForConfigOrDie(config)
}

func newELBv2Client(region string) elbv2iface.ELBV2API {
	config := aws.NewConfig().WithRegion(region)
	config = config.WithCredentialsChainVerboseErrors(true)
	config = request.WithRetryer(config, log.NewRetryLogger(DefaultRetryer))
	sess, err := session.NewSession(config)
	if err != nil {
		log.Fatalf("failed to create elbv2 client, %v", err)
	}

	return elbv2.New(sess)
}

func newELBClient(region string) elbiface.ELBAPI {
	config := aws.NewConfig().WithRegion(region)
	config = config.WithCredentialsChainVerboseErrors(true)
	config = request.WithRetryer(config, log.NewRetryLogger(DefaultRetryer))
	sess, err := session.NewSession(config)
	if err != nil {
		log.Fatalf("failed to create elb client, %v", err)
	}

	return elb.New(sess)
}

func newSQSClient(region string) sqsiface.SQSAPI {
	config := aws.NewConfig().WithRegion(region)
	config = config.WithCredentialsChainVerboseErrors(true)
	config = request.WithRetryer(config, log.NewRetryLogger(DefaultRetryer))
	sess, err := session.NewSession(config)
	if err != nil {
		log.Fatalf("failed to create sqs client, %v", err)
	}
	return sqs.New(sess)
}

func newASGClient(region string) autoscalingiface.AutoScalingAPI {
	config := aws.NewConfig().WithRegion(region)
	config = config.WithCredentialsChainVerboseErrors(true)
	config = request.WithRetryer(config, log.NewRetryLogger(DefaultRetryer))
	sess, err := session.NewSession(config)
	if err != nil {
		log.Fatalf("failed to create asg client, %v", err)
	}
	return autoscaling.New(sess)
}
