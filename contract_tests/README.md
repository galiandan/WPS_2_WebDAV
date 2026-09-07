# 契约金标准

这里保存脱敏后的 HTTP/WebDAV JSON 金标准，供 Go 服务的回归测试读取。生产服务不依赖本目录，金标准也不包含真实 Cookie、CSRF、签名 URL、账号 ID 或文件内容。

## 目录

- results/：基线响应和请求行为记录。
- results/go/：Go 服务对应的复核结果。

金标准覆盖健康检查、Basic Auth、REST、WebDAV、上传、下载、COPY、LOCK、递归 PROPFIND、空间状态和资源限制等场景。Go 测试会在 go/internal/httpserver/ 中直接加载这些 JSON，并对响应状态、正文和关键头部做断言。

## 运行

在仓库根目录执行：

~~~sh
cd go
go test ./...
~~~

测试只使用本地 fake storage 和静态金标准，不访问真实 WPS。

## 安全

不要向金标准中加入 Cookie、CSRF、refresh token、Basic Auth 密码、签名 URL、真实文件名、真实 ID 或用户文件内容。真实抓包只保存在本机 captures/，该目录已被 .gitignore 忽略。
