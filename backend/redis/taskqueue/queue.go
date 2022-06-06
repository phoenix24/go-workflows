package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type taskQueue[T any] struct {
	tasktype   string
	setKey     string
	streamKey  string
	groupName  string
	workerName string
}

type TaskItem[T any] struct {
	// TaskID is the generated ID of the task item
	TaskID string

	// ID is the provided id
	ID string

	// Optional data stored with a task, needs to be serializable
	Data T
}

var ErrTaskAlreadyInQueue = errors.New("task already in queue")

type TaskQueue[T any] interface {
	// Enqueue adds a task to the queue
	Enqueue(ctx context.Context, p redis.Pipeliner, id string, data *T) error

	// Dequeue returns the next task item from the queue. If no task is available, nil is returned
	Dequeue(ctx context.Context, rdb redis.UniversalClient, lockTimeout, timeout time.Duration) (*TaskItem[T], error)

	// Extend extends the lock of the given task item
	Extend(ctx context.Context, p redis.Pipeliner, taskID string) error

	// Complete completes the task with the given taskID
	Complete(ctx context.Context, p redis.Pipeliner, taskID string) error

	// Data returns the stored data for the given task
	Data(ctx context.Context, p redis.Pipeliner, taskID string) (*TaskItem[T], error)
}

func New[T any](rdb redis.UniversalClient, tasktype string) (TaskQueue[T], error) {
	tq := &taskQueue[T]{
		tasktype:   tasktype,
		setKey:     "task-set:" + tasktype,
		streamKey:  "task-stream:" + tasktype,
		groupName:  "task-workers",
		workerName: uuid.NewString(),
	}

	// Create the consumer group
	_, err := rdb.XGroupCreateMkStream(context.Background(), tq.streamKey, tq.groupName, "0").Result()
	if err != nil {
		// Ugly, check since there is no UPSERT for consumer groups. Might replace with a script
		// using XINFO & XGROUP CREATE atomically
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return nil, fmt.Errorf("creating task queue: %w", err)
		}
	}

	// Pre-load script
	enqueueCmd.Load(context.Background(), rdb)
	completeCmd.Load(context.Background(), rdb)

	return tq, nil
}

// KEYS[1] = stream
// KEYS[2] = stream
// ARGV[1] = caller provided id of the task
// ARGV[2] = additional data to store with the task
var enqueueCmd = redis.NewScript(
	// Prevent duplicates by checking a set first
	`local exists = redis.call("SADD", KEYS[1], ARGV[1])
	if exists == 0 then
		return nil
	end

	return redis.call("XADD", KEYS[2], "*", "id", ARGV[1], "data", ARGV[2])
`)

func (q *taskQueue[T]) Enqueue(ctx context.Context, p redis.Pipeliner, id string, data *T) error {
	ds, err := json.Marshal(data)
	if err != nil {
		return err
	}

	enqueueCmd.Run(ctx, p, []string{q.setKey, q.streamKey}, id, string(ds))

	return nil
}

func (q *taskQueue[T]) Dequeue(ctx context.Context, rdb redis.UniversalClient, lockTimeout, timeout time.Duration) (*TaskItem[T], error) {
	// Try to recover abandoned messages
	task, err := q.recover(ctx, rdb, lockTimeout)
	if err != nil {
		return nil, fmt.Errorf("checking for abandoned tasks: %w", err)
	}

	if task != nil {
		return task, nil
	}

	// Check for new tasks
	ids, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Streams:  []string{q.streamKey, ">"},
		Group:    q.groupName,
		Consumer: q.workerName,
		Count:    1,
		Block:    timeout,
	}).Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("dequeueing task: %w", err)
	}

	if len(ids) == 0 || len(ids[0].Messages) == 0 || err == redis.Nil {
		return nil, nil
	}

	msg := ids[0].Messages[0]
	return msgToTaskItem[T](&msg)
}

func (q *taskQueue[T]) Extend(ctx context.Context, p redis.Pipeliner, taskID string) error {
	// Claiming a message resets the idle timer. Don't use the `JUSTID` variant, we
	// want to increase the retry counter.
	_, err := p.XClaim(ctx, &redis.XClaimArgs{
		Stream:   q.streamKey,
		Group:    q.groupName,
		Consumer: q.workerName,
		Messages: []string{taskID},
		MinIdle:  0, // Always claim this message
	}).Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("extending lease: %w", err)
	}

	return nil
}

// We need TaskIDs for the stream and caller provided IDs for the set. So first look up
// the ID in the stream using the TaskID, then remove from the set and the stream
// KEYS[1] = set
// KEYS[2] = stream
// ARGV[1] = task id
// ARGV[2] = group
// We have to XACK _and_ XDEL here. See https://github.com/redis/redis/issues/5754
var completeCmd = redis.NewScript(`
	local task = redis.call("XRANGE", KEYS[2], ARGV[1], ARGV[1])
	if task == nil then
		return nil
	end
	local id = task[1][2][2]
	redis.call("SREM", KEYS[1], id)
	redis.call("XACK", KEYS[2], ARGV[2], ARGV[1])
	return redis.call("XDEL", KEYS[2], ARGV[1])
`)

func (q *taskQueue[T]) Complete(ctx context.Context, p redis.Pipeliner, taskID string) error {
	// Delete the task here. Overall we'll keep the stream at a small size, so fragmentation
	// is not an issue for us.
	err := completeCmd.Run(ctx, p, []string{q.setKey, q.streamKey}, taskID, q.groupName).Err()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("completing task: %w", err)
	}

	// TODO: Move this to the consumer?
	// if c.(int64) == 0 || err == redis.Nil {
	// 	return errors.New("could not find task to complete")
	// }

	return nil
}

func (q *taskQueue[T]) Data(ctx context.Context, p redis.Pipeliner, taskID string) (*TaskItem[T], error) {
	msg, err := p.XRange(ctx, q.streamKey, taskID, taskID).Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("finding task: %w", err)
	}

	return msgToTaskItem[T](&msg[0])
}

func (q *taskQueue[T]) recover(ctx context.Context, rdb redis.UniversalClient, idleTimeout time.Duration) (*TaskItem[T], error) {
	// Ignore the start argument, we are deleting tasks as they are completed, so we'll always
	// start this scan from the beginning.
	msgs, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   q.streamKey,
		Group:    q.groupName,
		Consumer: q.workerName,
		MinIdle:  idleTimeout,
		Count:    1,   // Get at most one abandoned task
		Start:    "0", // Start at the beginning of the pending items
	}).Result()

	if err != nil {
		return nil, fmt.Errorf("recovering tasks: %w", err)
	}

	if len(msgs) == 0 {
		return nil, nil
	}

	return msgToTaskItem[T](&msgs[0])
}

func msgToTaskItem[T any](msg *redis.XMessage) (*TaskItem[T], error) {
	id := msg.Values["id"].(string)
	data := msg.Values["data"].(string)

	var t T
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return nil, err
	}

	return &TaskItem[T]{
		TaskID: msg.ID,
		ID:     id,
		Data:   t,
	}, nil
}
