package test

import (
	"context"
	"testing"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/client"
	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/cschleiden/go-workflows/internal/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func BackendTest(t *testing.T, setup func() backend.Backend, teardown func(b backend.Backend)) {
	tests := []struct {
		name string
		f    func(t *testing.T, ctx context.Context, b backend.Backend)
	}{
		{
			name: "GetWorkflowTask_ReturnsNilWhenTimeout",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
				defer cancel()

				task, _ := b.GetWorkflowTask(ctx)
				require.Nil(t, task)
			},
		},
		{
			name: "GetActivityTask_ReturnsNilWhenTimeout",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
				defer cancel()

				task, _ := b.GetActivityTask(ctx)
				require.Nil(t, task)
			},
		},
		{
			name: "CreateWorkflowInstance_DoesNotError",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				instanceID := uuid.NewString()

				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: core.NewWorkflowInstance(instanceID, uuid.NewString()),
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.NoError(t, err)
			},
		},
		{
			name: "CreateWorkflowInstance_SameInstanceIDErrors",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				instanceID := uuid.NewString()
				executionID := uuid.NewString()

				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: core.NewWorkflowInstance(instanceID, executionID),
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.NoError(t, err)

				err = b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: core.NewWorkflowInstance(instanceID, executionID),
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.Error(t, err)
			},
		},
		{
			name: "GetWorkflowTask_ReturnsTask",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				wfi := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: wfi,
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.NoError(t, err)

				task, err := b.GetWorkflowTask(ctx)

				require.NoError(t, err)
				require.NotNil(t, task)
				require.Equal(t, wfi.InstanceID, task.WorkflowInstance.InstanceID)
			},
		},
		{
			name: "GetWorkflowTask_LocksTask",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				wfi := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: wfi,
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.Nil(t, err)

				// Get and lock only task
				task, err := b.GetWorkflowTask(ctx)
				require.NoError(t, err)
				require.NotNil(t, task)

				// First task is locked, second call should return nil
				ctx, cancel := context.WithTimeout(ctx, time.Millisecond*100)
				defer cancel()

				task, err = b.GetWorkflowTask(ctx)

				require.NoError(t, err)
				require.Nil(t, task)
			},
		},
		{
			name: "CompleteWorkflowTask_ReturnsErrorIfNotLocked",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				wfi := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: wfi,
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.NoError(t, err)

				err = b.CompleteWorkflowTask(ctx, &task.Workflow{}, wfi, backend.WorkflowStateActive, []history.Event{}, []history.Event{}, []history.WorkflowEvent{})

				require.Error(t, err)
			},
		},
		{
			name: "CompleteWorkflowTask_AddsNewEventsToHistory",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				startedEvent := history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{})
				activityScheduledEvent := history.NewPendingEvent(time.Now(), history.EventType_ActivityScheduled, &history.ActivityScheduledAttributes{}, history.ScheduleEventID(1))

				wfi := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: wfi,
					HistoryEvent:     startedEvent,
				})
				require.NoError(t, err)

				task, err := b.GetWorkflowTask(ctx)
				require.NoError(t, err)

				taskStartedEvent := history.NewPendingEvent(time.Now(), history.EventType_WorkflowTaskStarted, &history.WorkflowTaskStartedAttributes{})
				events := []history.Event{
					taskStartedEvent,
					startedEvent,
					activityScheduledEvent,
				}

				sequenceID := int64(1)
				for i := range events {
					sequenceID++
					events[i].SequenceID = sequenceID
				}

				activityEvents := []history.Event{
					activityScheduledEvent,
				}

				workflowEvents := []history.WorkflowEvent{}

				err = b.CompleteWorkflowTask(ctx, task, wfi, backend.WorkflowStateActive, events, activityEvents, workflowEvents)
				require.NoError(t, err)

				time.Sleep(time.Second)

				h, err := b.GetWorkflowInstanceHistory(ctx, wfi, nil)
				require.NoError(t, err)
				require.Equal(t, len(events), len(h))
				for i, event := range events {
					require.Equal(t, event.ID, h[i].ID)
					require.Equal(t, event.Type, h[i].Type)
					require.Equal(t, event.Attributes, h[i].Attributes)
				}
			},
		},
		{
			name: "SignalWorkflow_ErrorWhenInstanceDoesNotExist",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				c := client.New(b)
				err := c.SignalWorkflow(ctx, "does-not-exist", "signal", "value")
				require.Error(t, err)
				require.Equal(t, backend.ErrInstanceNotFound, err)
			},
		},
		{
			name: "CancelWorkflow_ErrorWhenInstanceDoesNotExist",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				c := client.New(b)
				err := c.CancelWorkflowInstance(ctx, core.NewWorkflowInstance(uuid.NewString(), uuid.NewString()))
				require.Error(t, err)
				require.Equal(t, backend.ErrInstanceNotFound, err)
			},
		},
		{
			name: "CancelWorkflow_AddsCancelEventToPendingEvents",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				c := client.New(b)
				instance := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
				startWorkflow(t, ctx, b, c, instance)

				err := c.CancelWorkflowInstance(ctx, instance)
				require.NoError(t, err)

				task, err := b.GetWorkflowTask(ctx)
				require.NoError(t, err)

				require.Equal(t, history.EventType_WorkflowExecutionCanceled, task.NewEvents[len(task.NewEvents)-1].Type)
			},
		},
		{
			name: "CompleteWorkflowTask_SendsInstanceEvents",
			f: func(t *testing.T, ctx context.Context, b backend.Backend) {
				c := client.New(b)
				instance := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())

				subInstance1 := core.NewSubWorkflowInstance(uuid.NewString(), uuid.NewString(), instance.InstanceID, 1)
				startWorkflow(t, ctx, b, c, subInstance1)

				// Create parent instance
				err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
					WorkflowInstance: instance,
					HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
				})
				require.NoError(t, err)

				// Simulate context and sub-workflow cancellation
				task, err := b.GetWorkflowTask(ctx)
				require.NoError(t, err)
				err = b.CompleteWorkflowTask(ctx, task, instance, backend.WorkflowStateActive, task.NewEvents, []history.Event{}, []history.WorkflowEvent{
					{
						WorkflowInstance: subInstance1,
						HistoryEvent: history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionCanceled, &history.SubWorkflowCancellationRequestedAttributes{
							SubWorkflowInstance: subInstance1,
						}),
					},
				})
				require.NoError(t, err)

				task, err = b.GetWorkflowTask(ctx)
				require.NoError(t, err)
				require.Equal(t, subInstance1, task.WorkflowInstance)
				require.Equal(t, history.EventType_WorkflowExecutionCanceled, task.NewEvents[len(task.NewEvents)-1].Type)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := setup()
			ctx := context.Background()
			tt.f(t, ctx, b)
			if teardown != nil {
				teardown(b)
			}
		})
	}
}

func startWorkflow(t *testing.T, ctx context.Context, b backend.Backend, c client.Client, instance *core.WorkflowInstance) {
	err := b.CreateWorkflowInstance(ctx, history.WorkflowEvent{
		WorkflowInstance: instance,
		HistoryEvent:     history.NewHistoryEvent(1, time.Now(), history.EventType_WorkflowExecutionStarted, &history.ExecutionStartedAttributes{}),
	})
	require.NoError(t, err)

	// Get task to clear initial event
	task, err := b.GetWorkflowTask(ctx)
	require.NoError(t, err)

	err = b.CompleteWorkflowTask(ctx, task, instance, backend.WorkflowStateActive, task.NewEvents, []history.Event{}, []history.WorkflowEvent{})
	require.NoError(t, err)
}
