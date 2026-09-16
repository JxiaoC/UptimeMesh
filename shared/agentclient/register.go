// 确保共享探测执行器被注册(HTTP 由 shared/probe 的 init 注册;票 08 在此追加 PING)。
package agentclient

import _ "github.com/uptimemesh/shared/probe"
