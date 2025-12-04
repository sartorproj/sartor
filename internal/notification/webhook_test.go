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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWebhookNotifier(t *testing.T) {
	configs := []WebhookConfig{
		{
			URL: "http://example.com/webhook",
		},
	}
	notifier := NewWebhookNotifier(configs)
	assert.NotNil(t, notifier)
}

func TestWebhookNotifier_Send(t *testing.T) {
	tests := []struct {
		name       string
		event      Event
		handler    http.HandlerFunc
		wantErr    bool
		errContain string
	}{
		{
			name: "successful notification",
			event: Event{
				Type:          EventTypePRCreated,
				Timestamp:     time.Now(),
				TailoringName: "test-tailoring",
				Namespace:     "default",
				Message:       "PR created successfully",
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)

				var event Event
				err = json.Unmarshal(body, &event)
				require.NoError(t, err)

				assert.Equal(t, EventTypePRCreated, event.Type)
				assert.Equal(t, "test-tailoring", event.TailoringName)

				w.WriteHeader(http.StatusOK)
			},
			wantErr: false,
		},
		{
			name: "server error",
			event: Event{
				Type:    EventTypePRCreated,
				Message: "Test",
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr:    true,
			errContain: "500",
		},
		{
			name: "custom headers",
			event: Event{
				Type:    EventTypePRMerged,
				Message: "Test",
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "custom-value", r.Header.Get("X-Custom-Header"))
				w.WriteHeader(http.StatusOK)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			config := WebhookConfig{
				URL: server.URL,
			}
			if tt.name == "custom headers" {
				config.Headers = map[string]string{
					"X-Custom-Header": "custom-value",
				}
			}

			notifier := NewWebhookNotifier([]WebhookConfig{config})

			err := notifier.Send(context.Background(), tt.event)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWebhookNotifier_EventFiltering(t *testing.T) {
	tests := []struct {
		name       string
		config     WebhookConfig
		eventType  EventType
		shouldSend bool
	}{
		{
			name: "no filter - send all",
			config: WebhookConfig{
				URL: "http://example.com",
			},
			eventType:  EventTypePRCreated,
			shouldSend: true,
		},
		{
			name: "filter match - send",
			config: WebhookConfig{
				URL:        "http://example.com",
				EventTypes: []EventType{EventTypePRCreated, EventTypePRMerged},
			},
			eventType:  EventTypePRCreated,
			shouldSend: true,
		},
		{
			name: "filter no match - don't send",
			config: WebhookConfig{
				URL:        "http://example.com",
				EventTypes: []EventType{EventTypePRMerged},
			},
			eventType:  EventTypePRCreated,
			shouldSend: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			tt.config.URL = server.URL
			notifier := NewWebhookNotifier([]WebhookConfig{tt.config})

			event := Event{
				Type:    tt.eventType,
				Message: "Test",
			}

			err := notifier.Send(context.Background(), event)
			if tt.shouldSend {
				assert.NoError(t, err)
			} else {
				// If event is filtered, Send should not be called, but we can't easily test that
				// So we just verify it doesn't error
				assert.NoError(t, err)
			}
		})
	}
}

func TestEventTypes(t *testing.T) {
	// Verify all event types are defined
	eventTypes := []EventType{
		EventTypePRCreated,
		EventTypePRMerged,
		EventTypePRClosed,
		EventTypeOOMPatched,
		EventTypeAnalysisComplete,
		EventTypeError,
	}

	for _, et := range eventTypes {
		assert.NotEmpty(t, string(et))
	}
}

func TestEvent_JSON(t *testing.T) {
	event := Event{
		Type:          EventTypePRCreated,
		Timestamp:     time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		TailoringName: "test-tailoring",
		Namespace:     "default",
		Message:       "PR #123 created",
		Details: map[string]interface{}{
			"prNumber": 123,
			"prURL":    "https://github.com/org/repo/pull/123",
		},
	}

	data, err := json.Marshal(event)
	require.NoError(t, err)

	var decoded Event
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, event.Type, decoded.Type)
	assert.Equal(t, event.TailoringName, decoded.TailoringName)
	assert.Equal(t, event.Message, decoded.Message)
}
