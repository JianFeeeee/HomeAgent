module gitcode.com/JianFeeeee/HomeAgent

go 1.25.0

require (
	github.com/mattn/go-sqlite3 v1.14.48
	github.com/yuin/gopher-lua v1.1.2
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/yanyiwu/gojieba v1.4.7

require github.com/yalue/onnxruntime_go v1.13.0

require gitcode.com/JianFeeeee/homeagent-sdk v0.7.2

replace gitcode.com/JianFeeeee/homeagent-sdk => ./third_party/homeagent-sdk
