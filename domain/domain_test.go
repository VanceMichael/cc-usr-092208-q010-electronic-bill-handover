package electronicbillhandover

import (
	"os"
	"testing"
)

func TestFixtureMatchesDomain(t *testing.T) {
	raw, err := os.ReadFile("../fixtures/domain.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if value.Domain != "electronic-bill-handover" {
		t.Fatalf("领域标识不一致: %s", value.Domain)
	}
}
