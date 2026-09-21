package electronicbillhandover

import (
    "os"
    "testing"
)

func loadFixture(t *testing.T) Record {
    t.Helper()
    raw, err := os.ReadFile("../fixtures/domain.json")
    if err != nil {
        t.Fatal(err)
    }
    value, err := Parse(raw)
    if err != nil {
        t.Fatal(err)
    }
    return value
}

func contains(items []string, want string) bool {
    for _, item := range items {
        if item == want {
            return true
        }
    }
    return false
}

func TestFixtureMatchesDomain(t *testing.T) {
    value := loadFixture(t)
    if value.Domain != "electronic-bill-handover" {
        t.Fatalf("领域标识不一致: %s", value.Domain)
    }
}

func TestFixtureCoversPerBillChecks(t *testing.T) {
    value := loadFixture(t)
    for _, want := range []string{"签发方", "当前持有人", "背书序列", "货物批次", "运输节点", "质押状态", "支付条件"} {
        if !contains(value.PerBillChecks, want) {
            t.Fatalf("每份提单校验项缺少: %s", want)
        }
    }
}

func TestFixtureCoversExceptionEvents(t *testing.T) {
    value := loadFixture(t)
    for _, want := range []string{"承运人离线回补", "改单", "拆单", "合单", "质押解除", "争议冻结", "重复回调"} {
        if !contains(value.ExceptionEvents, want) {
            t.Fatalf("异常事件缺少: %s", want)
        }
    }
}
