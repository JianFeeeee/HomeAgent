module gitcode.com/JianFeeeee/HomeAgent

go 1.25.0

require (
	github.com/mattn/go-sqlite3 v1.14.48
	github.com/yuin/gopher-lua v1.1.2
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/yanyiwu/gojieba v1.4.7

require gitcode.com/JianFeeeee/homeagent-sdk v0.0.0-20260708004841-e9bdcf9304b0 // direct

replace gitcode.com/JianFeeeee/homeagent-sdk => /tmp/opencode/homeagent-sdk-repo
