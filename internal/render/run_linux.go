package render

import (
	"fmt"
	"strings"

	"github.com/virgil724/capview/internal/capture"
)

// Run creates the viewer window and renders frames from mailbox until closed.
// Linux supports only the Vulkan renderer.
func Run(cfg Config, mailbox *capture.Mailbox) (FinalState, error) {
	if r := strings.ToLower(cfg.Renderer); r != "" && r != "vulkan" {
		return FinalState{}, fmt.Errorf("renderer %q is not available on Linux (use vulkan)", cfg.Renderer)
	}
	return runVulkan(cfg, mailbox)
}
