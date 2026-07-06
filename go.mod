module gitcode.com/JianFeeeee/HomeAgent

go 1.19

require (
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-sqlite3 v1.14.22
	github.com/yanyiwu/gojieba v1.4.7
	github.com/yuin/gopher-lua v1.1.2
	gitcode.com/JianFeeeee/homeagent-sdk v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

replace gitcode.com/JianFeeeee/homeagent-sdk => ./third_party/homeagent-sdk
