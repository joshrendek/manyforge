-- Reverse 0037: remove the mcp.invoke grant from agent_runtime, then the permission itself.
DELETE FROM role_permission
    WHERE permission_key = 'mcp.invoke';

DELETE FROM permission
    WHERE key = 'mcp.invoke';
