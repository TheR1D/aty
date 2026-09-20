package llm

import (
	"context"
	"crypto/rand"
	"fmt"
)

func (c *Client) Prewarm(ctx context.Context, transcript []Turn, thinking, agent bool) error {
	system := c.system
	if agent {
		system, thinking = c.agent, true
	}
	request := c.ask(contextMessages(system, transcript), false, thinking)
	if agent {
		request = withTools(request, c.availableTools())
	}
	body, err := c.post(ctx, c.protocol.warm(request))
	if err == nil {
		closeHTTP(body, true)
	}
	return err
}

func (c *Client) InvalidatePromptCache() {
	if c.cacheKey.Load() != nil {
		c.rotatePromptCacheKey()
	}
}

func (c *Client) rotatePromptCacheKey() {
	key := newPromptCacheKey()
	c.cacheKey.Store(&key)
}

func (c *Client) promptCacheKey() string {
	if key := c.cacheKey.Load(); key != nil {
		return *key
	}
	return ""
}

func oneOutputCacheRequest(request chatRequest) chatRequest {
	request.MaxOutputTokens = new(1)
	return request
}

func newPromptCacheKey() string {
	var random [16]byte
	_, _ = rand.Read(random[:]) // crypto/rand.Read always fills the buffer.
	return fmt.Sprintf("aty-%x", random[:])
}
