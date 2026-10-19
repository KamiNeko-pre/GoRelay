package knowledge

import "time"

const MaxBodyBytes = 2 << 20

// Input 是正文导入入口接收到的数据，还没有文档身份和内容版本。
type Input struct {
	IdentityNamespace string     `json:"-"` // Trusted acquisition projection only; generic imports retain Source identity.
	Source            string     `json:"source"`
	SourceKey         string     `json:"source_key"`
	URL               string     `json:"url,omitempty"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	Format            string     `json:"format"`
	FetchedAt         time.Time  `json:"fetched_at,omitempty"`
	PublishedAt       *time.Time `json:"published_at,omitempty"`
	DataVersion       string     `json:"data_version,omitempty"`
	TaskID            uint64     `json:"task_id,omitempty"`
	RecordKey         string     `json:"record_key,omitempty"`
}

// Document 在规范化正文之外增加稳定身份 ID 和内容版本 Revision。
// 后续持久化和索引更新使用这些字段，但不在本包执行。
type Document struct {
	ID          string     `json:"id"`
	Revision    string     `json:"revision"`
	Source      string     `json:"source"`
	SourceKey   string     `json:"source_key"`
	URL         string     `json:"url,omitempty"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Format      string     `json:"format"`
	FetchedAt   time.Time  `json:"fetched_at,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	DataVersion string     `json:"data_version,omitempty"`
	TaskID      uint64     `json:"task_id,omitempty"`
	RecordKey   string     `json:"record_key,omitempty"`
}
