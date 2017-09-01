package main

import (
	"fmt"
	"log"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

var (
	cliFrequency = kingpin.Flag("frequency", "How frequently to check for nodes to cleanup").Default("120s").OverrideDefaultFromEnvar("FREQUENCY").Duration()
	cliDryRun    = kingpin.Flag("dry", "Only log, don't delete nodes").Bool()
)

func main() {
	kingpin.Parse()

	meta := ec2metadata.New(session.New(), &aws.Config{})
	region, err := meta.Region()
	if err != nil {
		panic(err)
	}

	var (
		svc     = ec2.New(session.New(&aws.Config{Region: aws.String(region)}))
		limiter = time.Tick(*cliFrequency)
	)

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	for {
		<-limiter

		list, err := clientset.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			log.Println("Failed to lookup node list:", err)
			continue
		}

		for _, node := range list.Items {
			// If this instance is ready, we don't want to clean it up.
			ready, err := isReady(node.Status.Conditions)
			if err != nil {
				log.Println("Failed to check if instance is ready:", err)
				continue
			}

			if ready {
				log.Println("Node is ready, skipping:", node.ObjectMeta.Name)
				continue
			}

			// We don't want to clean up any running instances.
			running, err := isRunning(svc, node.Spec.ExternalID)
			if err != nil {
				log.Println("Failed to check if instance is running:", err)
				continue
			}

			if running {
				log.Println("Node is running, skipping:", node.ObjectMeta.Name)
				continue
			}

			if *cliDryRun {
				log.Println("Node would have been deleted, skipping:", node.ObjectMeta.Name)
				continue
			}

			err = clientset.CoreV1().Nodes().Delete(node.ObjectMeta.Name, &metav1.DeleteOptions{})
			if err != nil {
				log.Println("Failed to delete node:", err)
			}
		}
	}
}

// Helper function to check if a Kubernetes node is "Ready".
func isReady(conditions []v1.NodeCondition) (bool, error) {
	for _, condition := range conditions {
		if condition.Type != v1.NodeReady {
			continue
		}

		if condition.Status == v1.ConditionFalse {
			return true, nil
		}

		return false, nil
	}

	return false, fmt.Errorf("cannot find condition type: %s", v1.NodeReady)
}

// Helper function to check if an AWS node is "Running".
func isRunning(svc *ec2.EC2, id string) (bool, error) {
	resp, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			aws.String(id),
		},
	})
	if err != nil {
		return false, err
	}

	// If we have no reservations, then we can assume that the instance is terminated.
	if len(resp.Reservations) == 0 {
		return false, nil
	}

	for _, reservation := range resp.Reservations {
		for _, instance := range reservation.Instances {
			if *instance.InstanceId != id {
				continue
			}

			// We have found our running instance.
			if *instance.State.Name == ec2.InstanceStateNameRunning {
				return true, nil
			}

			return false, nil
		}
	}

	return false, fmt.Errorf("cannot find running instance: %s", id)
}
