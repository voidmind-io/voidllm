package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fastschema/qjs"
)

// sandboxPreamble removes dangerous global objects exposed by QuickJS's
// sandboxPreamble removes dangerous global objects exposed by QuickJS's std
// and os modules, replaces console with a capture buffer, and deletes timer
// APIs before user code executes. Console output is captured into __logs and
// returned in the execution result for debugging.
const sandboxPreamble = "delete globalThis.std;" +
	"delete globalThis.os;" +
	"delete globalThis.bjson;" +
	"delete globalThis.setTimeout;" +
	"delete globalThis.setInterval;" +
	"delete globalThis.clearTimeout;" +
	"delete globalThis.clearInterval;" +
	"delete globalThis.print;" +
	"globalThis.__logs = [];" +
	"globalThis.console = {" +
	"log: (...a) => __logs.push({level:'log',msg:a.map(String).join(' ')})," +
	"warn: (...a) => __logs.push({level:'warn',msg:a.map(String).join(' ')})," +
	"error: (...a) => __logs.push({level:'error',msg:a.map(String).join(' ')})," +
	"info: (...a) => __logs.push({level:'info',msg:a.map(String).join(' ')})," +
	"debug: (...a) => __logs.push({level:'debug',msg:a.map(String).join(' ')})" +
	"};\n"

// ToolCaller executes a single MCP tool call through the VoidLLM gateway,
// preserving authentication, access control, session management, metrics,
// and usage logging. serverAlias identifies the upstream MCP server, toolName
// is the tool to invoke, and args is the JSON-encoded arguments object.
type ToolCaller func(ctx context.Context, serverAlias, toolName string, args json.RawMessage) (json.RawMessage, error)

// ExecuteParams holds the input for a Code Mode script execution.
type ExecuteParams struct {
	// Code is the JavaScript source code to execute.
	Code string
	// ServerTools maps server alias to the list of tools available from that
	// server. Only these tools are injected into the sandbox.
	ServerTools map[string][]Tool
	// CallTool routes individual tool calls through the VoidLLM gateway.
	CallTool ToolCaller
	// MaxToolCalls is the maximum number of tool calls allowed per execution.
	// Zero means no limit.
	MaxToolCalls int
}

// ExecuteResult holds the output of a Code Mode script execution.
type ExecuteResult struct {
	// Result is the final return value of the script, JSON-encoded.
	Result json.RawMessage `json:"result"`
	// ToolCalls records every tool invocation made during execution.
	ToolCalls []ToolCallLog `json:"tool_calls"`
	// Logs captures console.log/warn/error/info/debug output from the script.
	// Each entry has "level" and "msg" fields.
	Logs []ConsoleLog `json:"logs"`
	// DurationMS is the total wall-clock execution time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Error is non-empty when the script failed (syntax error, timeout, OOM,
	// or unhandled exception).
	Error string `json:"error,omitempty"`
}

// ConsoleLog is a single console output captured from a Code Mode script.
type ConsoleLog struct {
	// Level is the console method that was called (log, warn, error, info, debug).
	Level string `json:"level"`
	// Msg is the concatenated string representation of all arguments.
	Msg string `json:"msg"`
}

// ToolCallLog records a single tool invocation made from within a Code Mode script.
type ToolCallLog struct {
	// Server is the MCP server alias.
	Server string `json:"server"`
	// Tool is the tool name as registered on the upstream server.
	Tool string `json:"tool"`
	// DurationMS is the tool call round-trip time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Status is "success" or "error".
	Status string `json:"status"`
}

// Executor runs Code Mode scripts in sandboxed QJS runtimes.
type Executor struct {
	pool *RuntimePool
}

// NewExecutor creates a Code Mode executor backed by the given runtime pool.
func NewExecutor(pool *RuntimePool) *Executor {
	return &Executor{pool: pool}
}

// Execute runs JavaScript code in a sandboxed QJS runtime with MCP tools
// injected as async functions. Returns the execution result including all
// tool call logs. The runtime is acquired from the pool and always discarded
// after execution to prevent cross-user JS global state leakage.
func (e *Executor) Execute(ctx context.Context, params ExecuteParams) (res *ExecuteResult, retErr error) {
	rt, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire runtime: %w", err)
	}

	// Always release with healthy=false. Runtimes are never reused across
	// executions — this prevents cross-user JS global state leakage.
	defer func() {
		e.pool.Release(rt, false)
	}()

	start := time.Now()

	result := &ExecuteResult{}

	var (
		logMu    sync.Mutex
		toolLogs []ToolCallLog
	)

	qctx := rt.Context()

	// toolCallCount tracks the number of tool calls made so far. The counter
	// is shared across all registered tool functions via closure.
	var toolCallCount atomic.Int64

	// Detect identifier collisions before registering any tool functions.
	// Two different alias/tool combinations could produce the same sanitized
	// function name (e.g. "foo-bar"/"baz" and "foo_bar"/"baz" both map to
	// "__tool_foo_bar_baz"). Collisions would silently overwrite one tool with
	// another, enabling tool impersonation.
	seen := make(map[string]string) // funcName → "alias/toolName"
	for alias, tools := range params.ServerTools {
		for _, tool := range tools {
			funcName := "__tool_" + sanitizeIdentifier(alias) + "_" + sanitizeIdentifier(tool.Name)
			key := alias + "/" + tool.Name
			if orig, exists := seen[funcName]; exists {
				result.Error = fmt.Sprintf("tool function name collision: %q and %q both map to %s", orig, key, funcName)
				return result, nil
			}
			seen[funcName] = key
		}
	}

	// Register one async function per tool. The name is a flat identifier
	// built from sanitized server alias and tool name so the JS preamble can
	// reference it without any object-property lookup.
	for alias, tools := range params.ServerTools {
		for _, tool := range tools {
			funcName := "__tool_" + sanitizeIdentifier(alias) + "_" + sanitizeIdentifier(tool.Name)

			// Capture loop variables before entering the closure.
			capturedAlias := alias
			capturedTool := tool.Name

			qctx.SetAsyncFunc(funcName, func(this *qjs.This) {
				// Enforce the per-execution tool call limit before doing any work.
				if params.MaxToolCalls > 0 && toolCallCount.Add(1) > int64(params.MaxToolCalls) {
					this.Promise().Reject(qctx.ThrowError(fmt.Errorf("tool call limit exceeded (max %d)", params.MaxToolCalls)))
					return
				}

				args := this.Args()

				// The preamble calls the function with JSON.stringify(args || {}),
				// so the first argument is always a JSON string. Free all argument
				// values after extracting the string to avoid leaking WASM memory.
				var rawArgs json.RawMessage
				if len(args) > 0 {
					s := args[0].String()
					if s != "" {
						rawArgs = json.RawMessage(s)
					}
				}
				for _, arg := range args {
					arg.Free()
				}
				if len(rawArgs) == 0 {
					rawArgs = json.RawMessage("{}")
				}

				callStart := time.Now()

				var (
					callResult json.RawMessage
					callErr    error
				)

				if params.CallTool != nil {
					callResult, callErr = params.CallTool(ctx, capturedAlias, capturedTool, rawArgs)
				} else {
					callErr = errors.New("no tool caller configured")
				}

				durationMS := time.Since(callStart).Milliseconds()

				status := "success"
				if callErr != nil {
					status = "error"
				}

				logMu.Lock()
				toolLogs = append(toolLogs, ToolCallLog{
					Server:     capturedAlias,
					Tool:       capturedTool,
					DurationMS: durationMS,
					Status:     status,
				})
				logMu.Unlock()

				if callErr != nil {
					this.Promise().Reject(qctx.ThrowError(callErr))
					return
				}

				this.Promise().Resolve(qctx.ParseJSON(string(callResult)))
			})
		}
	}

	preamble := buildToolsPreamble(params.ServerTools)
	// sandboxPreamble runs first to delete dangerous globals (std, os, console,
	// timers) before the tools preamble or user code can reference them.
	fullCode := sandboxPreamble + preamble + "\n" + params.Code

	// Recover from any panic inside Eval (e.g. WASM OOM) and mark the
	// runtime as unhealthy so the pool replaces it.
	defer func() {
		if r := recover(); r != nil {
			result.DurationMS = time.Since(start).Milliseconds()
			result.ToolCalls = snapshotToolLogs(&logMu, toolLogs)
			result.Error = fmt.Sprintf("runtime panic: %v", r)
			res = result
			retErr = nil
		}
	}()

	evalResult, evalErr := rt.Eval("code_mode.js", qjs.Code(fullCode), qjs.FlagAsync())

	result.DurationMS = time.Since(start).Milliseconds()
	result.ToolCalls = snapshotToolLogs(&logMu, toolLogs)
	result.Logs = extractConsoleLogs(rt)

	if evalErr != nil {
		result.Error = evalErr.Error()
		return result, nil
	}

	if evalResult != nil {
		// Always free the QJS value after extracting its content to avoid
		// leaking WASM memory. Values returned by Eval must be freed by
		// the caller per the fastschema/qjs memory management contract.
		encoded, encErr := valueToJSON(evalResult)
		evalResult.Free()
		if encErr != nil {
			result.Error = fmt.Sprintf("encode result: %s", encErr.Error())
		} else {
			result.Result = encoded
		}
	}

	return result, nil
}

// sanitizeIdentifier replaces characters that are not alphanumeric or
// underscores with underscores, producing a valid JavaScript identifier
// from an MCP server alias or tool name.
func sanitizeIdentifier(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// needsQuoting reports whether s must be quoted as a property key in a JS
// object literal (i.e. it is not a valid bare identifier).
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			// always valid in any position
		case r >= '0' && r <= '9':
			if i == 0 {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// buildToolsPreamble generates a JavaScript preamble that declares a `tools`
// constant whose structure mirrors the ServerTools map. Each leaf property is a
// thin wrapper that JSON.stringifies the caller-supplied argument object before
// forwarding it to the corresponding flat __tool_* async function.
func buildToolsPreamble(serverTools map[string][]Tool) string {
	if len(serverTools) == 0 {
		return "const tools = {};"
	}

	var sb strings.Builder
	sb.WriteString("const tools = {\n")

	for alias, tools := range serverTools {
		sanitizedAlias := sanitizeIdentifier(alias)

		var aliasKey string
		if needsQuoting(alias) {
			aliasKey = `"` + jsStringEscape(alias) + `"`
		} else {
			aliasKey = alias
		}

		sb.WriteString("  ")
		sb.WriteString(aliasKey)
		sb.WriteString(": {\n")

		for _, tool := range tools {
			sanitizedTool := sanitizeIdentifier(tool.Name)
			funcName := "__tool_" + sanitizedAlias + "_" + sanitizedTool

			var toolKey string
			if needsQuoting(tool.Name) {
				toolKey = `"` + jsStringEscape(tool.Name) + `"`
			} else {
				toolKey = tool.Name
			}

			sb.WriteString("    ")
			sb.WriteString(toolKey)
			sb.WriteString(": (args) => ")
			sb.WriteString(funcName)
			sb.WriteString("(JSON.stringify(args || {})),\n")
		}

		sb.WriteString("  },\n")
	}

	sb.WriteString("};")
	return sb.String()
}

// jsStringEscape returns s with characters escaped for use inside a JS
// double-quoted string literal. It delegates to json.Marshal which applies the
// same escaping rules as a JSON string value.
func jsStringEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	// json.Marshal wraps the string in double quotes; strip them.
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// valueToJSON converts a QJS *Value to a JSON-encoded byte slice. When the
// value is a JS string that is itself valid JSON (as produced by
// JSON.stringify inside the script), it is returned directly. Otherwise
// JSONStringify is called on the value to produce its JSON representation.
func valueToJSON(v *qjs.Value) (json.RawMessage, error) {
	if v.IsString() {
		s := v.String()
		if json.Valid([]byte(s)) {
			return json.RawMessage(s), nil
		}
		// The string is not valid JSON on its own — encode it as a JSON string.
		b, err := json.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("marshal string result: %w", err)
		}
		return json.RawMessage(b), nil
	}

	s, err := v.JSONStringify()
	if err != nil {
		return nil, fmt.Errorf("JSONStringify result: %w", err)
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("JSONStringify returned invalid JSON: %s", s)
	}
	return json.RawMessage(s), nil
}

// snapshotToolLogs returns a copy of the accumulated tool call logs under the
// provided mutex. It always returns a non-nil slice so that JSON serialisation
// produces an array rather than null.
func snapshotToolLogs(mu *sync.Mutex, logs []ToolCallLog) []ToolCallLog {
	mu.Lock()
	defer mu.Unlock()
	out := make([]ToolCallLog, len(logs))
	copy(out, logs)
	return out
}

// extractConsoleLogs reads the __logs array from the QJS global scope and
// returns it as a Go slice. The sandbox preamble installs a console replacement
// that pushes {level, msg} objects into __logs. Returns a non-nil empty slice
// if no logs were captured or if extraction fails.
func extractConsoleLogs(rt *qjs.Runtime) []ConsoleLog {
	logsVal, err := rt.Eval("__console_logs.js", qjs.Code("JSON.stringify(globalThis.__logs || [])"), qjs.FlagAsync())
	if err != nil {
		return []ConsoleLog{}
	}
	defer logsVal.Free()

	var logs []ConsoleLog
	if logsVal.IsString() {
		if jsonErr := json.Unmarshal([]byte(logsVal.String()), &logs); jsonErr != nil {
			return []ConsoleLog{}
		}
	}
	if logs == nil {
		logs = []ConsoleLog{}
	}
	return logs
}
