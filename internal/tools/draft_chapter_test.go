package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NightYuYyy/ainovel-cli/internal/domain"
	"github.com/NightYuYyy/ainovel-cli/internal/store"
)

func TestDraftChapterRejectsUnfinishedPendingRewrite(t *testing.T) {
	s := store.NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Progress.Init("test", 80); err != nil {
		t.Fatalf("Progress.Init: %v", err)
	}
	for ch := 1; ch <= 58; ch++ {
		if err := s.Progress.MarkChapterComplete(ch, 3000, "", ""); err != nil {
			t.Fatalf("MarkChapterComplete(%d): %v", ch, err)
		}
	}

	p, _ := s.Progress.Load()
	p.Flow = domain.FlowPolishing
	p.PendingRewrites = []int{65}
	if err := s.Progress.Save(p); err != nil {
		t.Fatalf("Save corrupt progress: %v", err)
	}

	tool := NewDraftChapterTool(s)
	args, err := json.Marshal(map[string]any{
		"chapter": 65,
		"content": "错误写入未来章节。",
		"mode":    "write",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "pending_rewrites 只能包含已完成章节") {
		t.Fatalf("expected invalid pending_rewrites rejection, got %v", err)
	}
	progress, _ := s.Progress.Load()
	if progress.InProgressChapter == 65 {
		t.Fatalf("future chapter should not become in progress")
	}
}
