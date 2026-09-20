package config

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func setup(input io.Reader, output io.Writer) (Backend, error) {
	_, _ = fmt.Fprintln(output, "ATY first launch setup")
	reader := bufio.NewReader(input)
	ask := func(label string, required bool) (string, error) {
		for {
			_, _ = fmt.Fprintf(output, "%s: ", label)
			line, err := reader.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("aty: setup stopped while reading %s: %w", label, err)
			}
			value := strings.TrimSpace(line)
			if required && value == "" {
				_, _ = fmt.Fprintln(output, "This field is required.")
				continue
			}
			return value, nil
		}
	}

	var backend Backend
	for {
		provider, err := ask("provider (llama, ollama, openai-compatible, openai-native)", true)
		if err != nil {
			return Backend{}, err
		}
		backend.Provider = Provider(provider)
		if backend.Provider == Llama || backend.Provider == Ollama || backend.Provider == OpenAICompatible || backend.Provider == OpenAINative {
			break
		}
		_, _ = fmt.Fprintln(output, "Choose llama, ollama, openai-compatible, or openai-native.")
	}
	var err error
	endpointLabel := "endpoint (full Chat Completions URL)"
	if backend.Provider == OpenAINative {
		endpointLabel = "endpoint (full Responses URL, e.g. https://api.openai.com/v1/responses)"
	}
	backend.Endpoint, err = ask(endpointLabel, true)
	if err != nil {
		return Backend{}, err
	}
	key, err := ask("api_key (can be empty for local models)", backend.Provider == OpenAICompatible || backend.Provider == OpenAINative)
	if err != nil {
		return Backend{}, err
	}
	backend.APIKey = &key
	backend.Model, err = ask("model", true)
	if err != nil {
		return Backend{}, err
	}
	effort, err := ask("reasoning_effort (e.g. low, medium, high. Model-dependent, Enter to skip)", false)
	if err != nil {
		return Backend{}, err
	}
	if effort != "" {
		backend.ReasoningEffort = &effort
	}
	return backend, nil
}
