package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// Holds the env variables
type Config struct {
	SlackWebhookURL   string
	SenderEmail       string
	RecipientEmail    string
	AWSRegion         string
	MonitoredServices []string
}

type ECSDeplomentDetail struct {
	EventName string `json:"eventName"`
	Cluster   string `json:"cluster"`
	Service   string `json:"service"`
	Reason    string `json:"reason"`
}

type ECSTaskDetail struct {
	ClusterArn    string          `json:"clusterArn"`
	TaskArn       string          `json:"taskArn"`
	Group         string          `json:"group"`
	LastStatus    string          `json:"lastStatus"`
	StoppedReason string          `json:"stoppedReason"`
	Containers    []ContainerInfo `json:"containers"`
}

type ContainerInfo struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason"`
}

var (
	sesClient *ses.Client
	cfg       Config
)

func init() {
	servicesEnv := os.Getenv("MONITORED_SERVICES")
	var servicesList []string
	if servicesEnv != "" {
		servicesList = strings.Split(servicesEnv, ",")
		// Trim spaces just in case
		for i := range servicesList {
			servicesList[i] = strings.TrimSpace(servicesList[i])
		}
	}
	// Load configuration from environment variables or a config file
	cfg = Config{
		SlackWebhookURL:   os.Getenv("SLACK_WEBHOOK_URL"),
		SenderEmail:       os.Getenv("SENDER_EMAIL"),
		RecipientEmail:    os.Getenv("RECIPIENT_EMAIL"),
		AWSRegion:         os.Getenv("AWS_REGION"),
		MonitoredServices: servicesList,
	}

	// Initialize AWS SDK
	awsCfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}

	// Create SES client
	sesClient = ses.NewFromConfig(awsCfg)
}

func handleRequest(ctx context.Context, event events.CloudWatchEvent) error {
	log.Printf("Received event: %v", event.DetailType)

	var message string
	var subject string
	isAlert := false

	switch event.DetailType {
	case "ECS Deployment State Change":
		var detail ECSDeplomentDetail
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			log.Printf("Error unmarshalling ECS deployment detail: %v", err)
			return err
		}
		message = fmt.Sprintf("ECS Deployment Event: %s\nCluster: %s\nService: %s\nReason: %s",
			detail.EventName, detail.Cluster, detail.Service, detail.Reason)
		subject = "ECS Deployment Alert"
		isAlert = true

		if detail.EventName == "SERVICE_DEPLOYMENT_FAILED" {
			isAlert = true
			subject = fmt.Sprintf("ECS Service Rollback/Failure: %s", getResourceName(detail.Service))
			message = fmt.Sprintf("*Service:* %s\n*Event:* %s\n*Reason:* %s\n*Cluster:* %s",
				getResourceName(detail.Service), detail.EventName, detail.Reason, getResourceName(detail.Cluster))
		}

	case "ECS Task State Change":
		var detail ECSTaskDetail
		if err := json.Unmarshal(event.Detail, &detail); err != nil {
			return fmt.Errorf("failed to unmarshal task detail: %v", err)
		}
		serviceName := getServiceNameFromGroup(detail.Group)

		// We only care if the task STOPPED and it wasn't a manual stop (exit code != 0)
		if detail.LastStatus == "STOPPED" {
			failedContainerFound := false
			failureDetails := ""

			for _, c := range detail.Containers {
				// ExitCode is an int, check if it's non-zero
				if c.ExitCode != 0 {
					failedContainerFound = true
					failureDetails += fmt.Sprintf("- Container '%s' exited with code %d (%s)\n", c.Name, c.ExitCode, c.Reason)
				}
			}

			// Also catch tasks that failed to start (no exit code, but stopped reason exists)
			if !failedContainerFound && detail.StoppedReason != "" && detail.StoppedReason != "Scaling activity initiated by (deployment ...)" {
				// Filter out normal scaling down events
				if !strings.Contains(detail.StoppedReason, "Scaling activity") && !strings.Contains(detail.StoppedReason, "Service scheduler") {
					failedContainerFound = true
					failureDetails += fmt.Sprintf("- Task stopped: %s\n", detail.StoppedReason)
				}
			}

			if failedContainerFound {
				isAlert = true
				subject = fmt.Sprintf("⚠️ ECS Task Failure: %s", serviceName)
				message = fmt.Sprintf("*Service:* %s\n*Task ARN:* %s\n*Failure Details:*\n%s",
					serviceName, detail.TaskArn, failureDetails)
			}
		}
	}
	var serviceName string
	if event.DetailType == "ECS Task State Change" {
		var detail ECSTaskDetail
		if err := json.Unmarshal(event.Detail, &detail); err == nil {
			serviceName = getServiceNameFromGroup(detail.Group)
		}
	} else if event.DetailType == "ECS Deployment State Change" {
		var detail ECSDeplomentDetail
		if err := json.Unmarshal(event.Detail, &detail); err == nil {
			serviceName = getResourceName(detail.Service)
		}
	}
	if len(cfg.MonitoredServices) > 0 {
		if !contains(cfg.MonitoredServices, serviceName) {
			log.Printf("Skipping alert for service '%s' (not in allowed list)", serviceName)
			return nil
		}
	}

	if isAlert {
		// Send Slack
		if err := sendSlackNotification(message); err != nil {
			log.Printf("Error sending Slack: %v", err)
		} else {
			log.Println("Slack notification sent")
		}

		// Send Email
		if err := sendEmail(subject, message); err != nil {
			log.Printf("Error sending Email: %v", err)
		} else {
			log.Println("Email notification sent")
		}
	} else {
		log.Println("Event processed, no alert conditions met.")
	}

	return nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func sendSlackNotification(text string) error {
	if cfg.SlackWebhookURL == "" {
		log.Println("Slack webhook URL not configured, skipping Slack notification")
		return nil
	}

	payload := map[string]string{"text": text}
	payloadBytes, err := json.Marshal(payload)

	resp, err := http.Post(cfg.SlackWebhookURL, "application/json", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to send Slack notification: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("received non-200 response from Slack: %s", resp.Status)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Slack API error: %s", resp.Status)
	}

	return nil
}

func sendEmail(subject, body string) error {
	if cfg.SenderEmail == "" || cfg.RecipientEmail == "" {
		log.Println("Sender or recipient email not configured, skipping email notification")
		return nil
	}

	input := &ses.SendEmailInput{
		Destination: &types.Destination{
			ToAddresses: []string{cfg.RecipientEmail},
		},
		Message: &types.Message{
			Body: &types.Body{
				Text: &types.Content{
					Data: aws.String(body),
				},
			},
			Subject: &types.Content{
				Data: aws.String(subject),
			},
		},
		Source: aws.String(cfg.SenderEmail),
	}

	_, err := sesClient.SendEmail(context.TODO(), input)
	return err
}

// Helper to extract "my-service" from "arn:aws:ecs:us-east-1:123:service/my-service"
func getResourceName(arn string) string {
	parts := strings.Split(arn, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return arn
}

// Helper to extract service name from group "service:my-service"
func getServiceNameFromGroup(group string) string {
	parts := strings.Split(group, ":")
	if len(parts) > 1 {
		return parts[1]
	}
	return "Unknown (Task run manually?)"
}

func main() {
	lambda.Start(handleRequest)
}
