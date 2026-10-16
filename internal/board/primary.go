package board

// The primary marker: issue #17. A primary that is gone, or that nothing can
// prove, must not be something a user discovers only because the workers went
// quiet. Reconciliation and the paths that probe the primary persist why the
// primary is not verified (task.Project.PrimaryAttention) and the board shows it.
//
// The marker is a presentation of that one persisted fact. Drawing it reads the
// store only: no Herdr call, no write, and no card moves because of it - a worker
// whose primary is gone is still in the stage its own facts put it in.

import (
	"context"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/state"
)

const (
	// primaryMarkerLabel opens the marker. It is words, not a glyph or a color, so
	// it reads the same on a terminal without either.
	primaryMarkerLabel       = "Primary not verified: "
	primaryMarkerInstruction = "Reattach the primary OMP session"
	// primaryMarkerMaxLines bounds the reason alone; the reattach instruction
	// adds at most two lines.
	primaryMarkerMaxLines            = 4
	primaryMarkerInstructionMaxLines = 2
)

// ProjectPrimaryAttention reads why the project's primary is not verified, or the
// empty string when nothing is recorded. It is a store read only, and the board
// is opened on a bound project, so a project the store does not know is a read
// error rather than "nothing recorded": the latter would hide a broken
// persistence invariant. A legacy project row written before the fact existed
// reads as nothing recorded.
func ProjectPrimaryAttention(ctx context.Context, store *state.Store, projectKey string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("board: no store")
	}
	project, err := store.Project(ctx, strings.TrimSpace(projectKey))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(project.PrimaryAttention), nil
}

// WithPrimaryAttention sets the reason the board marks its primary as not
// verified, and returns the board. An empty reason draws no marker.
func (b *Board) WithPrimaryAttention(reason string) *Board {
	if b != nil {
		b.primaryAttention = strings.TrimSpace(reason)
	}
	return b
}

// PrimaryAttention is the recorded reason the primary is not verified, or empty.
func (b *Board) PrimaryAttention() string {
	if b == nil {
		return ""
	}
	return b.primaryAttention
}

// primaryMarker draws the marker as at most primaryMarkerMaxLines lines, each
// within width, in the one accent the board reserves for what needs a human.
// With no recorded attention it draws nothing, so a healthy board is exactly what
// it was before the marker existed.
//
// The reason is recorded text, so it is folded onto lines like any other stored
// value (wrap drops what must never reach a terminal). A reason that does not fit
// is cut with a plain-ASCII ellipsis before the reattach instruction.
func primaryMarker(b *Board, width int, th Theme) []string {
	reason := b.PrimaryAttention()
	if reason == "" {
		return nil
	}
	reasonLines := wrap(primaryMarkerLabel+reason, width)
	if len(reasonLines) > primaryMarkerMaxLines {
		reasonLines = reasonLines[:primaryMarkerMaxLines]
		reasonLines[primaryMarkerMaxLines-1] = ellipsize(reasonLines[primaryMarkerMaxLines-1], width)
	}
	instructionLines := wrap(primaryMarkerInstruction, width)
	if len(instructionLines) > primaryMarkerInstructionMaxLines {
		instructionLines = instructionLines[:primaryMarkerInstructionMaxLines]
		last := len(instructionLines) - 1
		instructionLines[last] = ellipsize(instructionLines[last], width)
	}
	lines := append(reasonLines, instructionLines...)
	for i, line := range lines {
		lines[i] = th.paint(Attention, line)
	}
	return lines
}

// primaryMarkerBlock is the marker followed by one blank separator line, for a
// caller that puts it above its own content. With nothing recorded it is empty.
func primaryMarkerBlock(b *Board, width int, th Theme) []string {
	marker := primaryMarker(b, width, th)
	if len(marker) == 0 {
		return nil
	}
	return append(marker, "")
}

// ellipsize ends a line that has more text after it with "...", within width.
func ellipsize(line string, width int) string {
	const dots = "..."
	if width <= len(dots) {
		return clip(dots, width)
	}
	return clip(line, width-len(dots)) + dots
}
