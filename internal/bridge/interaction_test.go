package bridge

import (
	"encoding/json"
	"testing"
)

func TestApprovalCardCarriesOnlyAcceptAndDeclineActions(t *testing.T) {
	encoded, err := approvalCard("需要批准命令", "42")
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Body struct {
			Elements []struct {
				Columns []struct {
					Elements []struct {
						Tag       string `json:"tag"`
						Behaviors []struct {
							Type  string            `json:"type"`
							Value map[string]string `json:"value"`
						} `json:"behaviors"`
					} `json:"elements"`
				} `json:"columns"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(encoded), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Body.Elements) != 2 || len(card.Body.Elements[1].Columns) != 2 {
		t.Fatalf("approval card actions are malformed: %#v", card.Body.Elements)
	}
	for _, column := range card.Body.Elements[1].Columns {
		if len(column.Elements) != 1 || column.Elements[0].Tag != "button" || len(column.Elements[0].Behaviors) != 1 {
			t.Fatalf("approval card button is malformed: %#v", column.Elements)
		}
		behavior := column.Elements[0].Behaviors[0]
		if behavior.Type != "callback" || behavior.Value["request_id"] != "42" {
			t.Fatalf("approval callback = %#v", behavior)
		}
		if decision := behavior.Value["decision"]; decision != "accept" && decision != "decline" {
			t.Fatalf("unexpected decision %q", decision)
		}
	}
}

func TestSplitAnswersKeepsQuestionOrder(t *testing.T) {
	answers := splitAnswers("first answer\n2. second answer", 2)
	if len(answers) != 2 || answers[0] != "first answer" || answers[1] != "second answer" {
		t.Fatalf("answers = %#v", answers)
	}
}
