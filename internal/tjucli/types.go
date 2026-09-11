// Package tjucli 定义天津大学校园命令行工具的核心数据结构、统一响应信封与错误模型。
//
// 架构设计与 Agent 契约：
//  1. 结构化信封（Envelope）：无论成功或失败，均统一通过 SuccessEnvelope 或 FailureEnvelope 封装，
//     以便调用方（如 Pi 等 AI Agent）能够稳定进行 JSON 反序列化解析；
//  2. 纯净的标准输出：业务数据严格输出至 stdout，诊断与系统日志输出至 stderr，严禁将两者混淆；
//  3. 确定性错误编码：通过 CLIError 提供机器可读的 Code 以及标准的 Exit Code（2 为参数错误，1 为业务/运行时错误）。
package tjucli

import "fmt"

const (
	// DefaultBaseURL 天津大学课程共享平台默认上游地址。
	DefaultBaseURL = "https://cs.tjuse.com"
	// DefaultSearchPages 默认最大搜索目录扫描页数。
	DefaultSearchPages = 20
	// MaximumSearchPages 允许搜索的最大扫描页数上限。
	MaximumSearchPages = 100
	// DefaultSearchLimit 默认搜索结果条数上限。
	DefaultSearchLimit = 50
	// MaximumSearchLimit 允许返回的最大搜索结果条数。
	MaximumSearchLimit = 1000
	// DefaultDownloadBytes 默认单文件下载大小阈值限制（64 MiB）。
	DefaultDownloadBytes = int64(64 << 20)
	// MaximumDownloadBytes 允许单文件下载的最大硬上限（1 GiB）。
	MaximumDownloadBytes = int64(1 << 30)
)

// ItemKind 标识资源条目的分类类型（文件 file 或目录 folder）。
type ItemKind string

const (
	// KindFile 普通文件资源。
	KindFile ItemKind = "file"
	// KindFolder 目录/文件夹。
	KindFolder ItemKind = "folder"
)

// Item 表示文件或目录资源的通用元数据。
type Item struct {
	Name       string   `json:"name"`        // 文件或文件夹名称
	Path       string   `json:"path"`        // 规范化相对路径（以 / 开头）
	Kind       ItemKind `json:"kind"`        // 条目种类（file 或 folder）
	Size       int64    `json:"size"`        // 文件字节大小（目录为 0）
	ModifiedAt string   `json:"modified_at"` // 上游最后修改时间戳
}

// SuccessEnvelope 成功执行时的通用 JSON 响应信封。
type SuccessEnvelope struct {
	OK   bool `json:"ok"`   // 固定为 true
	Data any  `json:"data"` // 实际业务数据载荷（如 ListResult, SearchResult）
	Meta any  `json:"meta"` // 关联的元信息（如分页游标 next_cursor、搜索完整度标记）
}

// FailureEnvelope 执行失败时的通用 JSON 响应信封。
type FailureEnvelope struct {
	OK    bool            `json:"ok"`    // 固定为 false
	Error CLIErrorPayload `json:"error"` // 结构化错误详情
}

// CLIErrorPayload 包含机器可读的错误码与人类/LLM 可读的错误说明。
type CLIErrorPayload struct {
	Code    string `json:"code"`    // 稳定的机器错误代号（如 invalid_argument, not_found）
	Message string `json:"message"` // 详细的中文/英文错误说明
}

// CLIError 实现 error 接口，并携带进程退出码（ExitCode）。
type CLIError struct {
	Code     string // 错误标识
	Message  string // 错误描述
	ExitCode int    // 进程退出码（2 表示入参语法错误，1 表示业务/运行时错误）
}

// Error 实现 error 接口的字符串化方法。
func (e *CLIError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// NewFlagError 创建命令行参数错误（退出码固定为 2，提示 Agent 检查传入的选项与参数）。
func NewFlagError(message string) *CLIError {
	return &CLIError{Code: "invalid_argument", Message: message, ExitCode: 2}
}

// NewRuntimeError 创建运行时或上游网络错误（退出码固定为 1）。
func NewRuntimeError(code, message string) *CLIError {
	return &CLIError{Code: code, Message: message, ExitCode: 1}
}

// ListResult 目录浏览命令（tjucli course ls）的数据载荷。
type ListResult struct {
	Items []Item `json:"items"` // 当前目录下的资源条目列表
}

// ListMeta 目录浏览的分页元数据。
type ListMeta struct {
	NextCursor *string `json:"next_cursor"` // 下一页的分页游标，若无更多则为 nil
}

// SearchResult 资料搜索命令（tjucli course search）的数据载荷。
type SearchResult struct {
	Items []Item `json:"items"` // 匹配搜索条件的条目列表
}

// SearchMeta 搜索过程的统计与完整度元数据。
type SearchMeta struct {
	Scope        string `json:"scope"`         // 搜索范围描述
	PagesScanned int    `json:"pages_scanned"` // 实际扫描的上游页数
	Incomplete   bool   `json:"incomplete"`    // 是否达到页面限制而未完全遍历全库
}

// DownloadResult 文件下载命令（tjucli course download）的数据载荷。
type DownloadResult struct {
	LocalPath string `json:"local_path"` // 保存至本地工作区的相对/绝对路径
	Bytes     int64  `json:"bytes"`      // 实际下载并落盘的字节数
	SHA256    string `json:"sha256"`     // 下载文件的 SHA-256 校验和（全小写十六进制）
}

// VersionResult 版本查看命令（tjucli version）的数据载荷。
type VersionResult struct {
	Version string `json:"version"`          // 当前编译版本号
	Commit  string `json:"commit,omitempty"` // Git Commit 简短哈希
}

// CapabilitiesResult 自省功能查询命令（tjucli capabilities）的数据载荷。
// Agent 可通过此命令动态感知本 CLI 所支持的全部底层命令集合。
type CapabilitiesResult struct {
	Provider string   `json:"provider"` // 当前生效的服务提供方模式（local 或 remote）
	Commands []string `json:"commands"` // 开放调用的命令列表
}
