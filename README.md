# ATY (beta)

Another terminal/shell AI assistant, but with main focus on making AI feel fast and native to the shell. Works over SSH and other interactive shell sessions. ATY builds context from your terminal input/output, and helps without dragging you into another app or making you pipe LLM context around.

https://github.com/user-attachments/assets/413b91dc-430b-4dab-bf4f-2408190c7154

Model used in demo - [**gemma-4-26B-A4B-it:BF16**](https://huggingface.co/google/gemma-4-26B-A4B-it). Local models are recommended if your hardware can run them. Choose a model with low [TTFT](https://www.ibm.com/think/topics/time-to-first-token) to get fast responses.

You can also use cloud providers. For a responsive experience, choose one with low latency (HTTP + TTFT). 

> [!WARNING]  
> When using a cloud provider, be mindful of what you send: terminal commands and output may include secrets or private data. Also make sure your provider supports [Zero Data Retention (ZDR)](https://openai.com/index/offering-zero-data-retention-for-frontier-models/) and it is enabled for your requests.

## Install

ATY works in any terminal on **macOS** and **Linux** with `zsh` or `bash`. [Build from source](#build-from-source) or run install script:

```shell
curl -fsSL https://raw.githubusercontent.com/TheR1D/aty/main/install.sh | sh
```

After installation is complete start ATY by running:
```shell
aty
```

On first launch, ATY asks for your provider, endpoint, API key, model, and reasoning effort. Check configuration examples on [this page](https://github.com/TheR1D/aty/wiki/Config-Examples).

## Usage

Start with `?` and ask for what you need:

```shell
? show 5 largest files here
# ATY writes a command into your shell input
find . -type f -exec du -h {} + | sort -hr | head -n 5
```

ATY uses your recent commands and their output as context (running `clear` clears context), so you can just ask it to fix an error:

```shell
./some_script.sh
# -> permission denied: ./some_script.sh
? fix
chmod +x some_script.sh && ./some_script.sh
```

### Thinking mode

Use `??` for more complex commands (reasoning mode):

```text
?? rename photos in the current folder by date taken
```

### Agent mode

Use `???` for tasks that take several steps (agent mode):

```shell
??? start a Postgres container, create test db, table and few records
```

Agent mode can also use [MCP tools](#mcp-tools).

## Query options

### Skip terminal context

By default, ATY includes recent commands, their output, and conversation with each request. Add `x` to omit terminal context from a request:

```text
?x create a tar.gz archive of dist
??x write a command to batch-convert webp images to png
```

### YOLO mode

Add `!` to execute commands automatically:

```text
?! show what is listening on port 8080
???! find why the local server is unhealthy and restart it
```

ATY presses Enter when each complete command is ready. In agent mode, it also approves MCP tool calls. ATY does not sandbox commands. **Use `!` only when you trust the request, model, and context.**

Combine it with `x` as `?!x`, `??!x`, or `???!x`.

## Start automatically

To auto launch ATY for all your terminal sessions, add this to your `~/.zshrc` or `~/.bashrc`:

```sh
# Launch ATY only for interactive shells
if
    # The shell is interactive.
    [[ $- == *i* ]] &&
    # Input and output are connected to a terminal.
    [[ -t 0 && -t 1 ]] &&
    # ATY is not already running.
    [[ ${ATY_ACTIVE:-} != 1 ]] &&
    # The aty command is available.
    command -v aty >/dev/null 2>&1
then
    exec aty
fi
```

## Configuration

On first launch, ATY asks for your provider, endpoint, API key, model, and reasoning effort.

Configuration files are stored in `~/.config/aty`. If `XDG_CONFIG_HOME` is set, it uses `$XDG_CONFIG_HOME/aty` instead. Restart ATY after changing the configuration.

| File                | Purpose                                                    |
| ------------------- | ---------------------------------------------------------- |
| `config.toml`       | Model connection, color, and limits shared by all modes.   |
| `think_config.toml` | Optional separate model configuration for `??` and `???`.  |
| `default_prompt.sh` | System prompt for normal and thinking modes.               |
| `agent_prompt.sh`   | System prompt for agent mode.                              |
| `mcp/mcp.json`      | Extra tools available in agent mode.                       |

### Configuration parameters

Set your provider, model, and full endpoint URL in `config.toml`.

| Provider | API |
| --- | --- |
| `llama` | llama.cpp Chat Completions |
| `ollama` | Ollama Chat Completions |
| `openai-compatible` | OpenAI-compatible Chat Completions |
| `openai-native` | OpenAI Responses |

```toml
# ~/.config/aty/config.toml

# Required. Choose a provider from the table above.
provider = "ollama"

# Required. The model name or alias served by your provider.
model = "gemma-4-26b-a4b"

# Required. The full API URL, including the endpoint path.
endpoint = "http://127.0.0.1:11434/v1/chat/completions"

# Optional for local providers; required for both OpenAI providers.
api_key = "your-api-key"

# Optional reasoning level for ?? and ??? (all modes with openai-native).
# Accepted values depend on the model. Omit to use its default.
reasoning_effort = "medium"

# Optional response variation. Omit to use the provider's default.
temperature = 0.1

# Sends system message and current context to LLM after you type ? + space
# This essentially prewarms cache for LLM to reduce the wait after Enter.
# This should be mainly used for local LLMs with slow KV fills. Defaults to false.
prewarm = false

# Query text and cursor color. Defaults to orange, `none` disables both.
# Choices: orange, red, green, yellow, blue, purple, cyan, none.
color = "orange"

# Shared limits for every mode. Keep this table only in config.toml.
[limits]

# Start removing the oldest commands and their output when saved context exceeds this many bytes.
transcript_bytes = 496000

# When trimming starts, aim to reduce saved context to this many bytes.
transcript_keep_bytes = 446400

# How much to save for each command, including its output and related AI text.
# If the output is too long, cut out the middle: keep 20% from the start and 80% from the end.
command_bytes = 80000

# Keep this many bytes of each successful shell or MCP tool result sent to the agent.
# Oversized results use the same 20/80 split and marker as command capture.
tool_output_bytes = 80000

# Allow this many rounds of tool calls before asking the agent for a final answer.
# Each round can contain more than one tool call.
agent_steps = 30
```



By default, `?`, `??`, and `???` all use same config file `config.toml`. To use a different config file for thinking mode (`??` and `???`), create `think_config.toml` next to `config.toml`.

For example, to use OpenAI for thinking and agent modes:

```toml
# ~/.config/aty/think_config.toml
provider = "openai-native"
model = "gpt-6-astra"
endpoint = "https://api.openai.com/v1/responses"
api_key = "your-api-key"
reasoning_effort = "medium"
```

`openai-native` uses `store = false` and keeps the agent's conversation state locally, including encrypted reasoning returned between tool calls. It sends reasoning effort and temperature only when configured.

### Environment variables

Environment variables override TOML settings. Use `ATY_` plus the setting name in uppercase, like `ATY_MODEL` or `ATY_API_KEY`.

### System prompts

The prompt files are Bash scripts. At startup, ATY runs each script with Bash and uses the text it prints as the model's system prompt.

Edit `~/.config/aty/default_prompt.sh` for `?` and `??` (normal and reasoning modes), or `~/.config/aty/agent_prompt.sh` for `???`(agent mode). For example, `default_prompt.sh` could contain:

```bash
#!/usr/bin/env bash
cat <<EOF
Return only the shell command needed for the user's request.
The user's shell is $SHELL on $(uname -s).
The initial shell prompt is {{.PS1}}.
Do not include a shell prompt (PS1) in your response.
EOF
```

Bash fills in `$SHELL` and runs `uname -s` to get the operating system name. ATY replaces `{{.PS1}}`with your initial shell prompt (on ATY launch).

## MCP tools

MCP servers provide extra tools that agent mode (`???`) can call. Add them to `mcp/mcp.json` in your config directory. ATY connects when it starts.

This example shows a local server that ATY starts as a process and a remote server it connects to over HTTP.

```json
{
  "mcpServers": {
    "local": {
      "command": "/path/to/mcp-server",
      "args": ["/path/to/project"]
    },
    "remote": {
      "url": "https://example.com/mcp",
      "headers": {
        "Authorization": "Bearer ${env:MCP_TOKEN}"
      }
    }
  }
}
```



### Server settings

Each entry under `mcpServers` has a name you choose and these settings:


| Field     | What it does                                                                                                     |
| --------- | ---------------------------------------------------------------------------------------------------------------- |
| `command` | Program ATY starts for a local server. Use this or `url`.                                                        |
| `args`    | Optional list of arguments for the local program.                                                                |
| `env`     | Optional environment variables for the local program.                                                            |
| `envFile` | Optional file of `NAME=value` environment variables for the local program.                                       |
| `url`     | Streamable HTTP endpoint for a remote server. Use this or `command`.                                             |
| `headers` | Optional HTTP headers for a remote server, such as an authorization token.                                       |
| `type`    | Optional. Defaults to `stdio` with `command` or `http` with `url`. Remote servers also accept `streamable-http`. |


Local servers communicate through standard input and output. They inherit basic variables such as `PATH` and `HOME`. Pass other variables through `env` or `envFile`. If both define the same variable, `env` wins. Relative `envFile` paths start from the directory where you launched ATY.

Remote servers with headers must use HTTPS, except for local addresses such as `localhost` or `127.0.0.1`.

### Variables in server settings

ATY replaces these placeholders in commands, arguments, URLs, headers, `env` values, and `envFile` paths:


| Placeholder                  | Value                                                                         |
| ---------------------------- | ----------------------------------------------------------------------------- |
| `${env:NAME}`                | The environment variable `NAME` from the process running ATY. It must be set. |
| `${userHome}`                | Your home directory.                                                          |

### Running tools

ATY shows each MCP tool call's name and arguments before running it. Press Enter to approve, use the left and right arrow keys to scroll long arguments, or press Esc or Ctrl-C to cancel. Yolo mode approves calls automatically.

## Troubleshooting

Use `?#` to dump provider checks, prompts, tools, and current context.

```text
?#
```

ATY writes the dump to a private temporary file and puts a `cat` command in your shell to read it.

## Build from source

Requires Go 1.27.1 or newer.

```sh
git clone https://github.com/TheR1D/aty.git && cd aty && go build -o bin/aty ./cmd/aty
./bin/aty
```
