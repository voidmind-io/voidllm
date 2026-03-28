-- Migration: 0006_code_mode_execution_id.up.sql
-- Description: Add execution_id to group Code Mode tool calls from the same
-- execute_code invocation.

ALTER TABLE mcp_tool_calls ADD COLUMN code_mode_execution_id TEXT;

CREATE INDEX idx_mcp_tool_calls_execution
    ON mcp_tool_calls (code_mode_execution_id)
    WHERE code_mode_execution_id IS NOT NULL;
