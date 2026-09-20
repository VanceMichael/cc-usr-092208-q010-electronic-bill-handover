package electronicbillhandover

import (
    "encoding/json"
    "errors"
)

// Record 表示项目共享的领域资料。
type Record struct {
    Domain string `json:"domain"`
    Version int `json:"version"`
    SampleID string `json:"sample_id"`
    Actors []string `json:"actors"`
    Facts []string `json:"facts"`
    Constraints []string `json:"constraints"`
}

// Parse 读取并检查带版本的业务资料。
func Parse(raw []byte) (Record, error) {
    var value Record
    if err := json.Unmarshal(raw, &value); err != nil { return Record{}, err }
    if value.Domain == "" || value.Version < 1 || value.SampleID == "" || len(value.Actors) < 2 || len(value.Facts) < 2 || len(value.Constraints) < 2 {
        return Record{}, errors.New("共享资料缺少必要字段")
    }
    return value, nil
}
