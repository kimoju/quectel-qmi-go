# quectel-qmi-go

`quectel-qmi-go`（`github.com/kimoju/quectel-qmi-go`）是一个面向 Linux 的纯 Go QMI/QMUX 协议栈与蜂窝连接管理器，主要用于移远（Quectel）等高通（Qualcomm）4G/5G 蜂窝模组，并作为 [VoHive](https://github.com/kimoju/vohive) 的核心 QMI 驱动层同步演进。

它的定位不是“包装外部命令工具”，而是直接围绕 `/dev/cdc-wdm*` 与 `qmi-proxy` 实现非阻塞协议栈、设备发现、双栈/多 PDN 拨号管理、UIM 智能卡 APDU 访问、短信、IMS 与 VOICE 控制面能力。

## 项目亮点

- **纯 Go 原生实现**：核心协议与数据面配置不依赖 `qmicli` 或 `quectel-CM` 外部二进制；支持直连 `/dev/cdc-wdm*`，也支持通过 `qmi-proxy` 与系统 `ModemManager` 安全共用控制通道。
- **非阻塞高可靠传输层**：Linux 下基于非阻塞 FD + `ppoll` + 唤醒管道（wake pipe）+ 引用计数文件描述符与原子 QMUX 帧写入构建，模组热拔出（`ENODEV` / `EIO`）时可毫秒级唤醒所有挂起请求而不卡死协程。
- **分层清晰的架构设计**：
  - `pkg/qmi`：协议级 QMUX 客户端与各 QMI Service 强类型封装
  - `pkg/manager`：面向常驻服务的自动拨号、双栈重连、IP 轮换（`RotateIP`）、事件总线与高层查询接口
  - `pkg/netcfg`：基于 [`kimoju/netlink`](https://github.com/kimoju/netlink) 的 Raw-IP、QMAP 子接口、IPv4/IPv6 地址与 `FLAG_ONLINK` 路由配置

## 当前能力

### 已实现的核心 QMI Service

| Service | 能力概览 |
| --- | --- |
| `DMS` | 厂商/型号/固件版本、IMEI/序列号、运行模式（在线/低功耗/重置）、ICCID/IMSI、频段能力、MAC、用户数据 |
| `NAS` | 驻网状态、4G LTE 与 **5G NR (`5G-NSA` / `5G-SA`)** 信号测量与 `ConfigSignalInfoV2` 阈值订阅、网络拒绝码（Reject Indication）解析、搜网、PLMN/RAT 偏好、小区信息、EONS 运营商名（GSM-7/UCS-2） |
| `WDS` | IPv4 / IPv6 双栈独立 ClientID 拨号与断开、Runtime Settings、3GPP Profile 读写、流量统计、Bearer 查询、Autoconnect |
| `WDA` | Raw-IP / QMAP 数据链路聚合格式协商与配置 |
| `UIM` | 卡槽状态与热切换、PIN1/PIN2 校验（兼容标准 Session TLV）、透明/定长记录文件读取、SMSC (`EF_SMSP`) 解析、逻辑通道开关与长/短 APDU 透传（含 `61xx`/`6Cxx` 处理） |
| `WMS` | PDU 短信发送、读取、列举、删除、存储后发送、路由配置、MT ACK 与新短信事件通知 |
| `IMS` | IMS / VoLTE / VoWiFi / SMS over IMS 服务开关读取与配置 |
| `IMSA` | IMS 注册状态、各业务可用状态查询与状态变更 Indication |
| `IMSP` | IMS Enabler 状态查询 |
| `VOICE` | 语音呼叫发起/接听/挂断、DTMF、蜂窝交互式 USSD、补充业务与通话状态 Indication |

### `manager` 层高阶能力

- 自动拨号、健康探活与断线指数退避重连
- **IPv4 / IPv6 双栈与纯 IPv6 支持**（含 IPv6 SLAAC 兜底、`FLAG_ONLINK` 默认路由、双栈 DNS 聚合与一键 `RotateIP` 重拨换 IP）
- **`QMAP Mux` 多 PDN 并发拨号**（子接口独立维护，不会误关停共享父网卡）
- USB / PCIe（MHI）模组拓扑自动发现（精确匹配接口端点与 `bInterfaceNumber`）
- `WMS` 短信、`IMS/IMSA` 注册状态、`VOICE` 通话/USSD 统一事件回调桥接（无锁重入死锁风险）

## 当前边界

- 传输层面向 `/dev/cdc-wdm*` 字符设备及 `qmi-proxy` Unix 套接字；暂不包含基于内核 `QRTR` 的 `IMSDCM`（16-bit Service ID）。
- 运行时控制与网卡编排面向 Linux 宿主机或特权/授权容器环境（非 Linux 平台支持编译类型检查与协议层单元测试）。

## 目录结构

```text
quectel-qmi-go/
├── cmd/
│   ├── cm/         # 主连接管理 CLI
│   ├── dms-tool/   # DMS 调试工具
│   ├── info-tool/  # 综合设备/信号信息查询
│   ├── nas-tool/   # NAS 驻网与信号调试
│   ├── sms-tool/   # WMS 短信收发调试
│   ├── wda-tool/   # WDA 数据格式调试
│   └── wds-tool/   # WDS Profile 与拨号调试
├── pkg/
│   ├── device/     # Linux sysfs USB/PCIe 模组拓扑自动发现
│   ├── manager/    # 高层连接、重连、事件与业务管理器
│   ├── netcfg/     # Linux Netlink Raw-IP / QMAP / 路由与 DNS 配置
│   └── qmi/        # QMUX 非阻塞传输层与各 QMI Service 协议封装
└── go.mod
```

## 环境要求

- Linux（运行拨号与 `/dev/cdc-wdm*` 硬件访问）
- Go `1.24+`
- 可访问的 QMI 控制节点（如 `/dev/cdc-wdm0`）及对应网络接口（如 `wwan0`）
- 配置网卡地址与路由需具备 `root` 或 `CAP_NET_ADMIN` 权限

## 编译与安装

作为库引入：

```bash
go get github.com/kimoju/quectel-qmi-go@latest
```

编译内置 CLI：

```bash
git clone https://github.com/kimoju/quectel-qmi-go.git
cd quectel-qmi-go
go build -o quectel-qmi-go ./cmd/cm
```

## CLI 快速开始

### 基本用法

```bash
# 默认自动发现第一个模组，发起 IPv4/IPv6 双栈拨号
sudo ./quectel-qmi-go -s internet

# 指定网络接口与控制节点
sudo ./quectel-qmi-go -i wwan0 -d /dev/cdc-wdm0 -s internet

# 带 PAP/CHAP 认证拨号
sudo ./quectel-qmi-go -s myapn -u user -p pass -a 1

# 仅 IPv4 或仅 IPv6 拨号
sudo ./quectel-qmi-go -s internet -4
sudo ./quectel-qmi-go -s internet -6

# 指定 ProfileIndex 和 MuxID，发起 QMAP 多路 PDN 拨号
sudo ./quectel-qmi-go -s ims -n 2 -m 2
```

### 常用参数

| 参数 | 说明 |
| --- | --- |
| `-s` | APN 名称 |
| `-u` / `-p` | 拨号认证用户名 / 密码 |
| `-a` | 认证类型：`0=none`、`1=PAP`、`2=CHAP`、`3=PAP\|CHAP` |
| `-pin` | SIM 卡 PIN 码 |
| `-i` | 绑定的网络接口名（如 `wwan0`） |
| `-d` | QMI 控制设备路径（如 `/dev/cdc-wdm0`） |
| `-4` / `-6` | 仅启用 IPv4 / 仅启用 IPv6（均不传时默认启用双栈） |
| `-set-route` | 自动写入默认路由（默认关闭，便于嵌入自定义策略路由） |
| `-set-dns` | 自动更新系统 DNS（默认关闭） |
| `-n` | 3GPP PDN Profile 索引 |
| `-m` | `QMAP Mux ID` |
| `-v` | 输出详细调试日志 |
| `-version` | 输出版本信息 |

## 作为 Go 库使用

### 1. 自动发现与双栈拨号

```go
package main

import (
	"fmt"
	"log"

	"github.com/kimoju/quectel-qmi-go/pkg/device"
	"github.com/kimoju/quectel-qmi-go/pkg/manager"
	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

func main() {
	modems, err := device.Discover()
	if err != nil || len(modems) == 0 {
		log.Fatalf("discover modems failed: %v", err)
	}

	mgr := manager.New(manager.Config{
		Device:        modems[0],
		APN:           "internet",
		EnableIPv4:    true,
		EnableIPv6:    true,
		UseProxy:      true, // 可选：通过 qmi-proxy 与 ModemManager 共享控制口
		AutoReconnect: true,
	}, nil)

	mgr.OnConnect(func(s *qmi.RuntimeSettings) {
		fmt.Printf("connected: v4=%v v6=%v\n", s.IPv4Address, s.IPv6Address)
	})

	if err := mgr.Start(); err != nil {
		log.Fatal(err)
	}
	defer mgr.Stop()

	select {}
}
```

### 2. 仅启动 QMI 控制面（不发起数据拨号）

适合仅做设备监控、UIM/eSIM APDU 读写、短信收发或 VoWiFi SIM 鉴权的宿主进程：

```go
mgr := manager.New(manager.Config{
	Device:   modems[0],
	UseProxy: true,
	NoDial:   true,
	NoRoute:  true,
	NoDNS:    true,
}, nil)

if err := mgr.StartCore(); err != nil {
	log.Fatal(err)
}
defer mgr.Stop()

ctx := context.Background()
manufacturer, _ := mgr.GetManufacturer(ctx)
model, _ := mgr.GetModel(ctx)
serving, _ := mgr.GetServingSystem(ctx)
fmt.Println(manufacturer, model, serving.RegistrationState)
```

### 3. 短信收发与事件监听

```go
if err := mgr.SendSMS("+8613800138000", "hello from quectel-qmi-go"); err != nil {
	log.Fatal(err)
}

mgr.OnEvent(func(e manager.Event) {
	switch e.Type {
	case manager.EventConnected:
		fmt.Println("data connected")
	case manager.EventNewSMS:
		fmt.Printf("new sms index=%d storage=%d\n", e.SMSIndex, e.StorageType)
	case manager.EventIMSRegistrationStatus:
		fmt.Printf("ims status=%+v\n", e.IMSRegistration)
	case manager.EventVoiceCallStatus:
		fmt.Printf("voice calls=%+v\n", e.VoiceCalls)
	}
})
```

## `manager.Config` 关键配置项

| 字段 | 说明 |
| --- | --- |
| `Device` | `device.ModemDevice`，可由 `device.Discover()` 获取或显式构造注入 |
| `UseProxy` / `ProxyPath` / `ProxyExecutable` | 启用并自定义 `qmi-proxy` 连接参数（`UseProxy=true` 时自动跳过破坏多租户状态的全局 `CTL Sync`） |
| `APN` / `Username` / `Password` / `AuthType` | WDS 拨号 APN 与认证配置 |
| `EnableIPv4` / `EnableIPv6` | IPv4 / IPv6 协议栈开关 |
| `PINCode` | SIM PIN 自动解锁码 |
| `AutoReconnect` | 断线自动重连开关 |
| `NoDial` | 仅初始化 QMI 控制面，不发起 WDS 拨号 |
| `NoRoute` / `NoDNS` | 禁止自动修改宿主机默认路由 / 系统 DNS |
| `DisableWMSInd` / `DisableIMSAInd` / `DisableVOICEInd` | 按需关闭对应服务的异步事件订阅 |
| `ProfileIndex` / `MuxID` | 3GPP PDN Profile 索引与 `QMAP` 多路复用 ID |

## 开发与测试

```bash
go vet ./...
go test -race -count=1 ./...
```
