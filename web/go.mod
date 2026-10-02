// 独立模块边界：阻止根模块的 go build/vet/test ./... 扫描 web/node_modules 中第三方附带的 Go 源码。
module icloud-hme/web

go 1.26
