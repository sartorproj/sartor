/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// EventType represents the type of notification event.
type EventType string

const (
	// EventTypePRCreated indicates a PR was created.
	EventTypePRCreated EventType = "pr_created"
	// EventTypePRMerged indicates a PR was merged.
	EventTypePRMerged EventType = "pr_merged"
	// EventTypePRClosed indicates a PR was closed.
	EventTypePRClosed EventType = "pr_closed"
	// EventTypeOOMPatched indicates an OOM patch was applied.
	EventTypeOOMPatched EventType = "oom_patched"
	// EventTypeAnalysisComplete indicates analysis completed.
	EventTypeAnalysisComplete EventType = "analysis_complete"
	// EventTypeError indicates an error occurred.
	EventTypeError EventType = "error"
)

// Event represents a notification event.
type Event struct {
	// Type is the event type.
	Type EventType `json:"type"`
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`
	// Namespace is the resource namespace.
	Namespace string `json:"namespace"`
	// TailoringName is the name of the Tailoring.
	TailoringName string `json:"tailoringName,omitempty"`
	// CutName is the name of the Cut.
	CutName string `json:"cutName,omitempty"`
	// PRUrl is the URL of the PR.
	PRUrl string `json:"prUrl,omitempty"`
	// PRNumber is the PR number.
	PRNumber int `json:"prNumber,omitempty"`
	// Message is a human-readable message.
	Message string `json:"message"`
	// Details contains additional event details.
	Details map[string]interface{} `json:"details,omitempty"`
}

// WebhookConfig holds the configuration for a webhook endpoint.
type WebhookConfig struct {
	// URL is the webhook endpoint URL.
	URL string
	// Headers are additional headers to send.
	Headers map[string]string
	// Timeout is the request timeout.
	Timeout time.Duration
	// RetryCount is the number of retries on failure.
	RetryCount int
	// EventTypes specifies which events to send (empty means all).
	EventTypes []EventType
}

// WebhookNotifier sends notifications to webhook endpoints.
type WebhookNotifier struct {
	configs []WebhookConfig
	client  *http.Client
}

// NewWebhookNotifier creates a new WebhookNotifier.
func NewWebhookNotifier(configs []WebhookConfig) *WebhookNotifier {
	return &WebhookNotifier{
		configs: configs,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Send sends an event to all configured webhooks.
func (w *WebhookNotifier) Send(ctx context.Context, event Event) error {
	if len(w.configs) == 0 {
		return nil
	}

	var lastErr error
	for _, config := range w.configs {
		// Check if this webhook wants this event type
		if len(config.EventTypes) > 0 && !containsEventType(config.EventTypes, event.Type) {
			continue
		}

		if err := w.sendToWebhook(ctx, config, event); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// sendToWebhook sends an event to a single webhook.
func (w *WebhookNotifier) sendToWebhook(ctx context.Context, config WebhookConfig, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	retries := config.RetryCount
	if retries == 0 {
		retries = 3
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, config.URL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Sartor/1.0")
		for k, v := range config.Headers {
			req.Header.Set(k, v)
		}

		resp, err := w.client.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return fmt.Errorf("failed to send webhook after %d attempts: %w", retries+1, lastErr)
}

// containsEventType checks if a slice contains an event type.
func containsEventType(types []EventType, t EventType) bool {
	for _, et := range types {
		if et == t {
			return true
		}
	}
	return false
}

// NotificationManager manages notifications across multiple channels.
type NotificationManager struct {
	webhooks *WebhookNotifier
}

// NewNotificationManager creates a new NotificationManager.
func NewNotificationManager(webhookConfigs []WebhookConfig) *NotificationManager {
	return &NotificationManager{
		webhooks: NewWebhookNotifier(webhookConfigs),
	}
}

// NotifyPRCreated sends a notification when a PR is created.
func (m *NotificationManager) NotifyPRCreated(ctx context.Context, namespace, tailoringName, cutName, prURL string, prNumber int) error {
	return m.webhooks.Send(ctx, Event{
		Type:          EventTypePRCreated,
		Timestamp:     time.Now(),
		Namespace:     namespace,
		TailoringName: tailoringName,
		CutName:       cutName,
		PRUrl:         prURL,
		PRNumber:      prNumber,
		Message:       fmt.Sprintf("PR #%d created for %s", prNumber, tailoringName),
	})
}

// NotifyPRMerged sends a notification when a PR is merged.
func (m *NotificationManager) NotifyPRMerged(ctx context.Context, namespace, tailoringName, cutName, prURL string, prNumber int) error {
	return m.webhooks.Send(ctx, Event{
		Type:          EventTypePRMerged,
		Timestamp:     time.Now(),
		Namespace:     namespace,
		TailoringName: tailoringName,
		CutName:       cutName,
		PRUrl:         prURL,
		PRNumber:      prNumber,
		Message:       fmt.Sprintf("PR #%d merged for %s", prNumber, tailoringName),
	})
}

// NotifyPRClosed sends a notification when a PR is closed without merge.
func (m *NotificationManager) NotifyPRClosed(ctx context.Context, namespace, tailoringName, cutName, prURL string, prNumber int, ignored bool) error {
	message := fmt.Sprintf("PR #%d closed for %s", prNumber, tailoringName)
	if ignored {
		message = fmt.Sprintf("PR #%d closed with sartor-ignore for %s (Tailoring paused)", prNumber, tailoringName)
	}

	return m.webhooks.Send(ctx, Event{
		Type:          EventTypePRClosed,
		Timestamp:     time.Now(),
		Namespace:     namespace,
		TailoringName: tailoringName,
		CutName:       cutName,
		PRUrl:         prURL,
		PRNumber:      prNumber,
		Message:       message,
		Details: map[string]interface{}{
			"ignored": ignored,
		},
	})
}

// NotifyOOMPatched sends a notification when an OOM patch is applied.
func (m *NotificationManager) NotifyOOMPatched(ctx context.Context, namespace, workloadKind, workloadName, containerName, previousLimit, newLimit string) error {
	return m.webhooks.Send(ctx, Event{
		Type:      EventTypeOOMPatched,
		Timestamp: time.Now(),
		Namespace: namespace,
		Message:   fmt.Sprintf("OOM patch applied to %s/%s container %s: %s -> %s", workloadKind, workloadName, containerName, previousLimit, newLimit),
		Details: map[string]interface{}{
			"workloadKind":  workloadKind,
			"workloadName":  workloadName,
			"containerName": containerName,
			"previousLimit": previousLimit,
			"newLimit":      newLimit,
		},
	})
}

// NotifyAnalysisComplete sends a notification when analysis is complete.
func (m *NotificationManager) NotifyAnalysisComplete(ctx context.Context, namespace, tailoringName string, containers int, savingsEstimate string) error {
	message := fmt.Sprintf("Analysis complete for %s: %d containers analyzed", tailoringName, containers)
	if savingsEstimate != "" {
		message += fmt.Sprintf(", estimated savings: %s", savingsEstimate)
	}

	return m.webhooks.Send(ctx, Event{
		Type:          EventTypeAnalysisComplete,
		Timestamp:     time.Now(),
		Namespace:     namespace,
		TailoringName: tailoringName,
		Message:       message,
		Details: map[string]interface{}{
			"containersAnalyzed": containers,
			"savingsEstimate":    savingsEstimate,
		},
	})
}

// NotifyError sends a notification when an error occurs.
func (m *NotificationManager) NotifyError(ctx context.Context, namespace, tailoringName, cutName, errorMessage string) error {
	return m.webhooks.Send(ctx, Event{
		Type:          EventTypeError,
		Timestamp:     time.Now(),
		Namespace:     namespace,
		TailoringName: tailoringName,
		CutName:       cutName,
		Message:       errorMessage,
	})
}
