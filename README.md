# mcpterm

An MCP tool server that provides stateful, TUI-compatible terminal sessions.

This is a proof-of-concept using [mcp-go](https://github.com/dwrtz/mcp-go).

Works quite well with Claude Desktop.

## Usage

Clone the repo and run `make` to generate `bin/mcpterm`.

```
git clone https://github.com/dwrtz/mcpterm.git
cd mcpterm
make
```

Move the `mcpterm` binary to a directory in your PATH.

```
sudo mv bin/mcpterm /usr/local/bin/mcpterm
```

### Add to Claude Desktop

Edit your `claude_desktop_config.json` (on Mac it's in `~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "mcpterm": {
      "command": "mcpterm",
      "args": []
    }
  }
}
```

Restart Claude Desktop. Now Claude should see the 2 tools provided by `mcpterm`.

## Tools

`run`: Runs a command in a stateful terminal session. E.g. if you cd into a directory, subsequent commands will run in that directory.

`runScreen`: Runs a command or series of keystrokes and returns the screen output. Intended for TUI apps such as vim or a python REPL.

## Tips:

You may want to tell Claude to use following control sequences with the `runScreen` tool:

```go
"^X": "\x18", // Ctrl+X
"^O": "\x0F", // Ctrl+O
"^J": "\x0A", // Enter
"^C": "\x03", // Ctrl+C
"^D": "\x04", // Ctrl+D
"^Z": "\x1A", // Ctrl+Z
"^[": "\x1B", // Escape
"^H": "\x08", // Backspace
"^M": "\x0D", // Carriage return
"^L": "\x0C", // Form feed
"^G": "\x07", // Bell
"^U": "\x15", // Clear line
"^W": "\x17", // Delete word
"^Y": "\x19", // Paste from kill buffer
"^V": "\x16", // Literal input
"^K": "\x0B", // Kill line
"^E": "\x05", // End of line
"^A": "\x01", // Beginning of line
"^I": "\x09", // Tab
```

While not perfect, it works pretty well. Claude is able to use vim to write a Dockerfile, then run the container in an interactive session, then run commands in the container such as a python REPL.
