package redis

import (
	"context"
	"encoding/json"

	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/go-redis/redis/v8"
)

func addEventToStreamP(ctx context.Context, p redis.Pipeliner, streamKey string, event *history.Event) error {
	eventData, err := json.Marshal(event)
	if err != nil {
		return err
	}

	p.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		ID:     "*",
		Values: map[string]interface{}{
			"event": string(eventData),
		},
	})

	return nil
}

// addEventsToStream adds the given events to the given event stream. If successful, the message id of the last event added
// is returned
// KEYS[1] - stream key
// ARGV[1] - event data as serialized strings
var addEventsToStreamCmd = redis.NewScript(`
	local msgID = ""
	for i = 1, #ARGV do
		msgID = redis.call("XADD", KEYS[1], "*", "event", ARGV[i])
	end
	return msgID
`)

func addEventsToStreamP(ctx context.Context, p redis.Pipeliner, streamKey string, events []history.Event) error {
	eventsData := make([]string, 0)
	for _, event := range events {
		eventData, err := json.Marshal(event)
		if err != nil {
			return err
		}

		eventsData = append(eventsData, string(eventData))
	}

	addEventsToStreamCmd.Run(ctx, p, []string{streamKey}, eventsData)

	return nil
}

// KEYS[1] - future event zset key
// KEYS[2] - future event key
// ARGV[1] - timestamp
// ARGV[2] - event payload
var addFutureEventCmd = redis.NewScript(`
	redis.call("ZADD", KEYS[1], ARGV[1], KEYS[2])
	redis.call("SET", KEYS[2], ARGV[2])
`)

func addFutureEventP(ctx context.Context, p redis.Pipeliner, instance *core.WorkflowInstance, event *history.Event) error {
	futureEvent := &futureEvent{
		Instance: instance,
		Event:    event,
	}

	eventData, err := json.Marshal(futureEvent)
	if err != nil {
		return err
	}

	addFutureEventCmd.Run(
		ctx,
		p,
		[]string{futureEventsKey(), futureEventKey(instance.InstanceID, event.ScheduleEventID)},
		event.VisibleAt.Unix(),
		string(eventData),
	)

	return nil
}

// KEYS[1] - future event zset key
// KEYS[2] - future event key
var removeFutureEventCmd = redis.NewScript(`
	redis.call("ZREM", KEYS[1], KEYS[2])
	redis.call("DEL", KEYS[2])
`)

func removeFutureEventP(ctx context.Context, p redis.Pipeliner, instance *core.WorkflowInstance, event *history.Event) {
	key := futureEventKey(instance.InstanceID, event.ScheduleEventID)
	removeFutureEventCmd.Run(ctx, p, []string{futureEventsKey(), key})
}
