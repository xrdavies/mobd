import { createMcpExtension } from "@earendil-works/pi-coding-agent";

// Supplied per process, never persisted in the user's Pi configuration.
export default function (pi) {
  const config = JSON.parse(process.env.MOB_PI_MCP_CONFIG);
  const approve = process.env.MOB_PI_PERMISSIONS === "approve";
  delete process.env.MOB_PI_MCP_CONFIG;
  delete process.env.MOB_PI_PERMISSIONS;

  // Reuse Pi's MCP transport without loading personal or project MCP servers.
  createMcpExtension({
    loadConfig: () => ({ servers: [], errors: [], autoEnableCodemode: false }),
  })(pi);
  pi.registerMcpServer("mob-mcp", config);

  // The runner checks this command before submitting any user prompt. An extension
  // load error or an unavailable MCP server must not silently remove the gate.
  pi.registerCommand("mob-ready", {
    description: "Check MOB integration readiness",
    handler: async () => {
      const required = ["screenshot", "ask_user", "request_approval"];
      const deadline = Date.now() + 10000;
      while (true) {
        const names = new Set(pi.getAllTools().map((tool) => tool.name));
        if (required.every((name) => names.has(`mcp__mob_mcp__${name}`))) return;
        if (Date.now() >= deadline) throw new Error("MOB MCP tools are unavailable");
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
    },
  });

  const readTools = new Set(["read", "grep", "find", "ls"]);
  pi.on("tool_call", async (event, ctx) => {
    if (!approve || readTools.has(event.toolName) || event.toolName.startsWith("mcp__mob_mcp__")) return;
    // Same desktop policy as the other runners: desktop/ask tools are allowed;
    // shell and file mutations use the provider gate, including unknown tools.
    if (!ctx.hasUI || !(await ctx.ui.confirm(`Allow Pi to use ${event.toolName}?`, JSON.stringify(event.input)))) {
      return { block: true, reason: "Tool call was not approved" };
    }
  });
}
