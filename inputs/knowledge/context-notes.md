# GoRelay 正文导入样本

这是一份手工编写的 Markdown 验证样本，不冒充网络采集数据。

## 文档身份与版本

来源名称与来源内的稳定标识决定文档 ID；同一份资料修改后，文档 ID
不变，正文版本改变。后续索引可以据此判断向量是否仍对应当前正文。

## 原文保留

正文保留段落和代码块，不用列表摘要代替全文：

```go
ctx, cancel := context.WithCancel(parent)
defer cancel()
```

这个入口只生成经过校验的正文 JSON。MySQL 持久化、切分、Embedding 和
Qdrant 索引属于后续步骤，不由这个样本宣称完成。
