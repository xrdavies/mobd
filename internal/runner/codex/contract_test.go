package codex

import (
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/runner/runnertest"
)

const codexReply = `cat > prompt.txt
printf '%s\n' "$@" > args.txt
printf '%s\n' '{"type":"thread.started","thread_id":"rid-1"}' \
  '{"type":"item.completed","item":{"id":"t1","type":"mcp_tool_call","server":"mob-mcp","tool":"screenshot","arguments":{},"result":{"content":[{"type":"text","text":"ok"}]},"error":null,"status":"completed"}}' \
  '{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"hello"}}' \
  '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

func TestContract(t *testing.T) {
	runnertest.Run(t, runnertest.Suite{
		New: func(bin, dir string) runner.Runner {
			return New(config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp"})
		},
		PromptOnStdin: true,
		Scripts: runnertest.Scripts{
			Reply: codexReply,
			Fail:  `cat > /dev/null; printf '%s\n' '{"type":"turn.failed","error":{"message":"boom from codex"}}'` + "\n",
			Hang:  `printf '%s\n' '{"type":"thread.started","thread_id":"rid-1"}'; sleep 30` + "\n",
		},
	})
}
