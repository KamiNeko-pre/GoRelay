package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Consumer reads new messages assigned to one Consumer Group member.
type Consumer struct {
	client    *redis.Client
	config    Config
	processor MessageProcessor
}

// NewConsumer validates dependencies without starting a goroutine.
func NewConsumer(client *redis.Client, cfg Config, processor MessageProcessor) (*Consumer, error) {
	if client == nil {
		return nil, fmt.Errorf("Redis client is required")
	}
	if processor == nil {
		return nil, fmt.Errorf("message processor is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate consumer config: %w", err)
	}
	return &Consumer{client: client, config: cfg, processor: processor}, nil
}

// Run blocks until ctx is cancelled or reading/processing a message fails.
func (consumer *Consumer) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		streams, err := consumer.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumer.config.Group,
			Consumer: consumer.config.Consumer,
			Streams:  []string{consumer.config.Stream, ">"},
			Count:    consumer.config.ReadCount,
			Block:    consumer.config.ReadBlock,
			NoAck:    false,
		}).Result()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read stream %q (group %q, consumer %q): %w", consumer.config.Stream, consumer.config.Group, consumer.config.Consumer, err)
		}
		for _, stream := range streams {
			if len(stream.Messages) <= 1 {
				for _, message := range stream.Messages {
					if err := consumer.processor.ProcessNewMessage(ctx, message); err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						log.Printf("message %s left pending in %s: %v", message.ID, stream.Stream, err)
					}
				}
				continue
			}
			var wg sync.WaitGroup
			for _, message := range stream.Messages {
				msg := message
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := consumer.processor.ProcessNewMessage(ctx, msg); err != nil {
						if ctx.Err() != nil {
							return
						}
						log.Printf("message %s left pending in %s: %v", msg.ID, stream.Stream, err)
					}
				}()
			}
			wg.Wait()
		}
	}
}
