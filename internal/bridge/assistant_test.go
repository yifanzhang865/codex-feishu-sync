package bridge

import (
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

func TestAssistantOutputPrefersDeltasAndFallsBackToCompletedItem(t *testing.T) {
	if got := assistantOutput("delta text", "completed text"); got != "delta text" {
		t.Fatalf("assistantOutput() = %q, want delta text", got)
	}
	if got := assistantOutput("", "completed text"); got != "completed text" {
		t.Fatalf("assistantOutput() = %q, want completed item text", got)
	}
}

func TestAssistantFallbackCoversFailedStreamsWithoutDuplicatingDeliveredItems(t *testing.T) {
	tests := []struct {
		name                    string
		bound                   bool
		streamDelivered         bool
		hasAssistant            bool
		assistantFullyDelivered bool
		want                    bool
	}{
		{name: "after turn without item", bound: true, want: true},
		{name: "successful stream without final item", bound: true, streamDelivered: true, want: false},
		{name: "failed stream without delivered item", bound: true, hasAssistant: true, want: true},
		{name: "failed stream with delivered final item", bound: true, hasAssistant: true, assistantFullyDelivered: true, want: false},
		{name: "partially delivered assistant items", bound: true, hasAssistant: true, want: true},
		{name: "unbound thread", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shouldFallbackAssistant(true, test.bound, test.streamDelivered, test.hasAssistant, test.assistantFullyDelivered)
			if got != test.want {
				t.Fatalf("shouldFallbackAssistant() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestItemTextIncludesAggregatedCommandOutput(t *testing.T) {
	if got := itemText(map[string]any{"type": "commandExecution", "aggregatedOutput": "build output"}); got != "build output" {
		t.Fatalf("itemText() = %q, want aggregated command output", got)
	}
}

func TestCodexItemLifecycleIsDeduplicated(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	item := map[string]any{"id": "item-1", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "请运行测试"}}}
	first, err := markCodexItem(store, "thread-a", item)
	if err != nil || !first {
		t.Fatalf("first item notification = %v, %v; want accepted", first, err)
	}
	second, err := markCodexItem(store, "thread-a", item)
	if err != nil || second {
		t.Fatalf("second item notification = %v, %v; want duplicate", second, err)
	}
}
