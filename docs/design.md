# go-wado-client 设计方案

> 目标：基于 Go 1.26 实现 DICOM WADO 客户端，支持 **WADO-RS** 与 **WADO-URI** 两种检索标准。
> 状态：设计方案（待评审），未开始编码。
> 标准依据：DICOM PS3.18（Web Services），调研基于 current 版（2026d），并同时考虑现网已部署服务器的经典行为（≤2022 版）。

---

## 1. 背景与目标

对接多家医院的 PACS / 影像网关，通过 DICOMweb 检索影像：

- **WADO-RS**（RESTful，multipart/related 流式传输）用于整 Study / Series / Instance / 帧数据 / 元数据 / 渲染图检索。
- **WADO-URI**（经典单 GET + query 参数）用于兼容老设备/老网关的单实例检索与 JPEG 渲染。

**关键业务约束（本次设计的核心输入）**：标准只定义了 URL 中"资源路径"的后半段（如 `/studies/{study}/series/{series}`），**不约束服务前缀**。每家医院前面的路由段各不相同（如 `api/xxx/xxx/{hospitalCode}`）。因此：

> URL = 可配置的 BaseURL（业务层按医院注入） + 标准固定的资源路径段（库内拼接）。

### 1.1 设计原则

1. **库不感知业务**：医院编码、前缀解析、鉴权凭证管理属于业务层；库只接收一个规范化后的 BaseURL。
2. **零第三方依赖**：核心库仅用标准库（`net/http`、`mime/multipart`、`net/url`、`encoding/json`、`iter`）。
3. **流式优先**：整 Study 可达数 GB，绝不整体载入内存；所有响应以 `io.Reader`/迭代器暴露。
4. **对现实宽容**：现网服务器实现质量参差（老版参数命名、multipart 细节不规范、自签证书），兼容策略显式建模，而不是隐式打补丁。

---

## 2. 标准调研结论

PS3.18 在 2023 年重组过章节，现行结构：第 8 章 = DICOM Web 服务公共约定（媒体类型语法、协商、公共状态码）；第 9 章 = **URI Service（WADO-URI）**；第 10 章 = **Studies Service and Resources（含 WADO-RS 的 Retrieve 事务）**。

### 2.1 WADO-RS（Retrieve 事务，PS3.18 §10.4）

全部为 **GET**，资源路径层级固定：

| 资源 | URI 模板 | 响应形态 |
|---|---|---|
| Study 全量实例 | `/studies/{study}` | multipart/related，每 part 一个 DICOM 文件（application/dicom） |
| Series 实例 | `/studies/{study}/series/{series}` | 同上 |
| 单实例 | `/studies/{study}/series/{series}/instances/{instance}` | 同上（单 part） |
| Study/Series/Instance 元数据 | `.../metadata` | application/dicom+json（新版单段；老版服务器可能仍用 multipart 包裹） |
| 帧像素数据 | `.../instances/{instance}/frames/{frames}` | multipart/related，type=application/octet-stream |
| 渲染图 | `.../rendered`、`.../frames/{frames}/rendered` | 单段 image/jpeg、image/png 等 |
| 缩略图（新增） | `.../thumbnail`、`.../frames/{frames}/thumbnail` | 渲染媒体类型 |
| Bulk Data | `.../bulkdata` 及元数据中返回的 `{bulkdataURI}` | multipart，type=application/octet-stream |
| 像素数据（新增） | `.../pixeldata` | multipart，octet-stream |
| MPR/3D 体渲染（新增，可选） | `.../renderedmpr`、`.../rendered3d` | 渲染媒体类型 |

要点：

- **`{study}`/`{series}`/`{instance}` 是 UID 原样出现在路径段中**；`{frames}` 为逗号分隔、升序帧号（1 起，现行文本不含区间语法）。
- **媒体类型协商走 `Accept` 头**（双端必选支持）：实例检索 `multipart/related; type="application/dicom"`，可附加 `transfer-syntax=<UID>` 参数协商转码（`*` 为标准定义的通配符，表示"服务器任选支持的传输语法"；该参数仅适用于实例/帧检索，metadata 恒为 dicom+json 不携带）；元数据 `application/dicom+json`；帧/批量数据 `multipart/related; type="application/octet-stream"`。新版另定义了 `accept`/`charset` 查询参数，老服务器只认头，库统一走 `Accept` 头（兼容面最大）。
- **渲染媒体类型**（§8.7.4）：单帧 image/jpeg（基准必须支持）/png/gif/jp2/jxl，多帧 gif/jxl，视频 video/mpeg、video/mp4、video/H265，文本 text/html、text/plain、application/pdf 等；渲染类型**不允许**带 transfer-syntax 参数。
- **渲染查询参数**：新版为 `annotation`、`quality`、`viewport`（vw,vh 即 columns,rows）、`window=center,width,function`（**三值必填**，function 取 linear/linear-exact/sigmoid，缺省 linear）、`iccprofile`；经典部署为 `annotations`、`quality`、`viewport`、`windowcenter`、`windowwidth`、`icccolorspace`（命名差异见 §3 D6）。
- **BulkDataURI 三种形式**（相对路径解析规则，见 §3 D9）：绝对 URI；带绝对路径的相对 URI（`/dicomweb/studies/...`）；带相对路径的相对 URI（`./bulkdata/...`，相对于**发起元数据请求的路径**）。
- **状态码**：200；400（请求/参数错误，如 UID 含非法字符）；404（资源不存在）；406（媒体类型不可协商）；410（已删除）；413（过大）；另有 §8.5 公共码（304/501/503 等）。
- multipart 每 part 通常带 `Content-Location`（指向该实例的资源 URI，可用于命名落盘文件）。

### 2.2 WADO-URI（URI Service，PS3.18 §9）

- **单一 GET，标准不定义任何路径**——Target URI 就是服务的 Base URI（现网常见 `/wado`、`/wado-uri` 等，完全由部署方决定）。资源全部通过查询参数标识：
  - 必选：`requestType=WADO`、`studyUID`、`seriesUID`、`objectUID`。
  - 可选（Retrieve DICOM Instance 事务，§9.4）：`contentType`（默认/缺省 `application/dicom`）、`charset`、`anonymize=yes`（经典名 `anonymity`）、`annotation=patient,technique`、`transferSyntax=<UID>`（转码，服务器可选支持）。
  - 可选（Retrieve Rendered Instance 事务，§9.5）：`contentType`（必须是渲染类型，填 `application/dicom` 会 406）、`frameNumber`、`imageQuality`、`rows`/`columns`（成对）、`region=xmin,ymin,xmax,ymax`（归一化坐标）、`windowCenter`/`windowWidth`（成对，与 Presentation State 互斥）、`presentationUID`/`presentationSeriesUID`（成对）。
- **响应恒为单段**（非 multipart）：一个 DICOM PS3.10 文件或一张渲染图。
- 缺省 `contentType` 且 `Accept: */*` 时默认 `image/jpeg`。
- 状态码：400（如 `requestType` 缺失或非 `WADO`）及 §8.5 公共码。
- **命名备忘（跟踪用）**：现行表 9.5.1-1 将渲染事务的注释参数 Key 写作 `imageAnnotation`，但同节 URL 模板仍为 `{&annotation}`，§9.4.1.2.2 亦为 `annotation`，dcm4chee 实现亦为 `annotation`——属标准文本内部不一致，库按 `annotation` 实现，跟踪后续 CP。`transferSyntax` 同样支持通配符 `*`。

### 2.3 新旧版本差异汇总（兼容策略的依据）

| 项 | 经典部署（≤2022 版及绝大多数现网 PACS） | 现行 2026d |
|---|---|---|
| WADO-RS 渲染参数名 | `annotations` `windowcenter` `windowwidth` `icccolorspace` | `annotation` `window` `iccprofile` |
| WADO-URI 匿名化 | `anonymity=yes` | `anonymize=yes` |
| metadata 响应 | 可能 multipart 包裹 dicom+json | 单段 dicom+json |
| 资源集合 | 无 thumbnail / pixeldata / MPR / 3D | 新增（均可选） |

库以**经典命名为默认**（现网主流），新版命名与任意私有参数通过透传机制支持（§3 D6）。

---

## 3. 关键设计决策

### D1. URL 组装：可配置 BaseURL + 固定标准路径段（对应核心业务约束）

```go
c, err := wado.NewClient("https://pacs.hosp-a.cn/api/gateway/H0001")   // 前缀任意，业务层给
c, err := wado.NewClient("https://10.20.30.40:8443/dicomweb")
```

- `NewClient` 解析并规范化 BaseURL：校验 scheme/host、去掉尾部 `/`，保留中间全部前缀段。
- 资源 URL 一律用 `(*url.URL).JoinPath("studies", studyUID, "series", seriesUID, ...)` 拼接——自动处理斜杠合并、`./`、`../` 清理与段转义。
- **前缀对库完全不透明**：库不解析、不假设 BaseURL 的内部结构——医院编码在第几段、段名今天叫 `hospitalCode` 明天叫 `orgId`、中间多一段版本号，都是业务侧的知识与数据，不是库的代码。契约只有一条：**BaseURL 以标准资源路径（`studies/...`）的直接父级结尾**。
- **UID 白名单校验**（进入路径前强制）：非空、总长 ≤ 64、仅 `[0-9.]`、无空组件、组件无前导零（单独 `0` 除外）。作用：① 防路径注入；② 把服务器端 400 提前为客户端错误，附带清晰报错。提供 `WithLenientUID()` 逃生门（个别私有网关接受非标 UID）。
- **多医院共享一个 Client 底座（Fork）**：`Client` 实例只是配置载体，不持有连接；连接池/TLS 会话都在共享的 `*http.Transport` 上，因此多实例零额外开销。针对"同一网关按医院/租户/业务等标识区分前缀"的场景（标识语义与库无关）：

  ```go
  gw, _ := wado.NewClient("https://gw.example.com/api", opts...)  // 底座：只定传输/鉴权/日志，前缀不完整没关系

  h1, _ := gw.Fork("https://gw.example.com/api/imaging/H0001/dicomweb") // 共享底座全部装配，仅换 BaseURL
  h2, _ := gw.Fork("https://gw.example.com/api/imaging/H0002/dicomweb", wado.WithBasicAuth(...)) // 可按院覆盖鉴权
  ```

  Fork 是浅拷贝 + 换 BaseURL（+ 可选覆盖项），不做任何网络操作。**刻意不做**"每次调用传 hospitalCode"的 API：医院码与 StudyUID 配错是取错数据的隐蔽事故，"先按医院拿到 Client、再检索"的两步式把绑定关系固定在一处。

### D2. WADO-URI 端点独立配置

标准未定义 WADO-URI 的路径，且同一医院 RS 与 URI 端点常不同（如 RS 在 `/dicom-web`，URI 在 `/wado`）：

```go
wado.WithWADOURIPath("/wado")                          // 相对 BaseURL 同 host（默认值，可改）
wado.WithWADOURIPath("https://gw.hosp-b.cn/wado-uri")  // 也允许完整 URL（跨网关）
```

请求 = 该端点 + `url.Values.Encode()` 生成的查询串，值全量转义。

### D3. multipart/related 流式解析

- 从响应 `Content-Type` 用 `mime.ParseMediaType` 提取 `boundary` 与 `type`（各 part 的媒体类型）。
- `multipart.NewReader(resp.Body, boundary)` 直接绑定 HTTP 响应体 → **边下载边消费，内存占用与 Study 大小无关**。
- 容错：`type` 参数缺失时降级用首个 part 的 `Content-Type`；容忍大小写/空格差异（`mime` 包天然容忍）。
- 大 part 不经缓冲直读（`*Part` 本身是 `io.Reader`，可直接 `io.Copy` 落盘）。

### D4. 传输层：不设整体 Timeout，用 Transport 细粒度超时

整 Study 下载可能远超任意整体超时。默认 Transport：

```go
ResponseHeaderTimeout: 30s   // 首字节/响应头超时
TLSHandshakeTimeout:   10s
IdleConnTimeout:       90s
// 不设 http.Client.Timeout —— 总时长由调用方用 context 控制
```

gzip 不手动设置 `Accept-Encoding`，交给 Transport 自动协商与透明解压（multipart 场景关键：手工设头会拿到未解压流）。

### D5. 鉴权与可观测性全部做成插拔点，不绑定任何一家

```go
wado.WithBasicAuth(user, pass)
wado.WithBearerTokenSource(ts)                       // ts: Token() (string, error)，失败自动重取
wado.WithRequestEditor(func(*http.Request) error)     // 任意签名/私有头
wado.WithHTTPClient(*http.Client)                     // 整体接管（含代理、TLS 配置）
wado.WithTLSClientConfig(*tls.Config)                 // 医院自签 CA 常见，单独给快捷方式
wado.WithTransportWrapper(func(rt http.RoundTripper) http.RoundTripper) // 接 otel/metrics，不硬依赖
wado.WithLogger(Logger)                               // 极简接口；nil = 静默
```

### D6. 参数命名兼容：经典默认 + 显式透传

- 渲染参数、匿名化参数默认发经典命名（现网主流）。
- `WithModernParamNames()` 切换新版命名（`annotation`/`window`/`iccprofile`/`anonymize`）。
- 任何场景可用 `WithRawQuery(url.Values)` 附加/覆盖查询参数，兜底所有私有网关。

### D7. 错误模型

```go
type StatusError struct {
    StatusCode int          // 400/404/406/410/413...
    Status     string
    Method, URL string
    Header     http.Header
    Body       []byte      // 截断至 4KB，便于排查网关报文
}
// 便捷断言：IsNotFound() / IsNotAcceptable() / IsGone() / IsRetryable() ...
// 客户端本地校验失败用独立错误类型（ErrInvalidUID、ErrInvalidRequest），携带字段名
```

### D8. 重试（默认关闭）

GET 幂等，`WithRetry(RetryPolicy{MaxAttempts, InitialBackoff, MaxBackoff, Jitter})` 只对网络错误、429、502/503/504 重试，尊重 `Retry-After`（封顶于 MaxBackoff，防止异常网关拖死客户端）与 context 取消；`Jitter` 为每次重试附加的随机抖动上界（防惊群，0 关闭）。

### D9. BulkDataURI 相对路径解析（标准三种形式）

```go
ResolveBulkDataURI(metaRequestURL *url.URL, uri string) (*url.URL, error)
// 绝对 URI                → 原样
// "/dicomweb/studies/..." → 取元数据请求 URL 的 scheme+host，拼绝对路径
// "./bulkdata/00282000"   → 相对【发起元数据请求的那个路径】解析
// "dicomweb://..." 带协议的相对形式 → 标准明确不支持，报错
```

### D10. 元数据（application/dicom+json）模型

独立子包 `dicomjson`，按 VR 提供类型化取值，不引入 DICOM 解析库：

```go
type Element struct {
    VR          string          `json:"vr"`
    Value       json.RawMessage `json:"Value,omitempty"`
    InlineBinary string         `json:"InlineBinary,omitempty"` // base64
    BulkDataURI  string         `json:"BulkDataURI,omitempty"`
}
type Dataset map[string]Element   // key = "00080018" 式 Tag

func (d Dataset) String(tag string) (string, bool)   // PN/LO/UI...
func (d Dataset) Int(tag string) (int, bool)         // IS/SL...
func (d Dataset) Float(tag string) (float64, bool)   // DS/FL...
func (d Dataset) Sequence(tag string) ([]Dataset, bool)
func (d Dataset) PersonName(tag string) (string, bool) // "PN^PN" 取首段
```

`Value` 的 JSON 形态随 VR 变化（字符串数组/数值数组/嵌套对象），保留 RawMessage + 按 VR 解码，避免万能 `any` 的类型断言泥潭。解析用 `json.Decoder` 流式（大 Study 元数据可达数十 MB）。

---

## 4. 总体架构与包结构

```
github.com/cocosip/go-wado-client/    (module github.com/cocosip/go-wado-client, go 1.26)
├── logger.go / option.go / core.go   // 根包 wado：共享底座 Core/Option/重试/slog 日志
├── errors.go / uid.go / urlx.go      // StatusError/RequestError、UID 白名单、BaseURL/引用解析
├── wadors/                         // WADO-RS 客户端（标准后缀拼接、multipart 流式游标）
│   ├── client.go options.go retrieve.go multipart.go
│   └── metadata.go frames.go rendered.go bulkdata.go
├── wadouri/                        // WADO-URI 客户端（全量参数 + 本地成对/互斥校验）
├── dicomx/                         // 可选：go-dicom parser 流式桥接 + go-dicom-codecs 空导入注册
└── multi/                          // 多目标注册表：泛型 Resolver（键语义归业务）→ Gateway 缓存
```

分层：`业务层（医院路由/凭证） → Client（URL 策略+Option 装配） → 请求构造（Accept 协商/参数编码/UID 校验） → 传输（鉴权/重试/日志 RoundTripper 链） → 响应解码（multipart/dicom+json）`。

## 5. 核心 API（草案）

### 5.1 WADO-RS

```go
func NewClient(baseURL string, opts ...Option) (*Client, error)

// —— 实例检索（multipart 流式）——
func (c *Client) RetrieveStudy(ctx context.Context, studyUID string, opts ...RetrieveOption) (*Multipart, error)
func (c *Client) RetrieveSeries(ctx context.Context, studyUID, seriesUID string, opts ...RetrieveOption) (*Multipart, error)
func (c *Client) RetrieveInstance(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RetrieveOption) (*Multipart, error)

// —— 元数据（dicom+json）——
func (c *Client) StudyMetadata(ctx context.Context, studyUID string, opts ...MetaOption) ([]dicomjson.Dataset, error)
func (c *Client) SeriesMetadata(ctx context.Context, studyUID, seriesUID string, opts ...MetaOption) ([]dicomjson.Dataset, error)
func (c *Client) InstanceMetadata(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...MetaOption) (*dicomjson.Dataset, error)

// —— 帧与渲染 ——
func (c *Client) RetrieveFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RetrieveOption) (*Multipart, error)
func (c *Client) RetrieveRenderedInstance(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RenderedOption) (*Rendered, error)
func (c *Client) RetrieveRenderedFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RenderedOption) (*Rendered, error)

// —— Bulk Data（uri 可为元数据里的 BulkDataURI，相对/绝对均可）——
func (c *Client) FetchBulkData(ctx context.Context, uri string, opts ...RetrieveOption) (io.ReadCloser, error)
```

`RetrieveOption`：`WithTransferSyntax(uid)`、`WithAccept(...)`、`WithCharset(cs)`。
`RenderedOption`：`WithRenderedFormat("image/png")`、`WithViewport(w,h)`、`WithQuality(n)`、`WithWindow(c,w)`、`WithAnnotation(patient, technique bool)`、`WithICCProfile(...)`、`WithRawQuery(url.Values)`。

### 5.2 multipart 游标（Go 1.23+ 迭代器风格）

```go
type Multipart struct{ /* ... */ }
func (m *Multipart) Parts() iter.Seq2[*Part, error]   // for p, err := range mp.Parts() { ... }
func (m *Multipart) ReadAll() ([][]byte, error)        // 小数据便捷
func (m *Multipart) WriteToDir(dir string) ([]string, error) // 文件名取 Content-Location 尾段 UID，兜底序号
func (m *Multipart) Close() error

type Part struct{ /* io.Reader + Header/ContentType/Location */ }
func (p *Part) Read(b []byte) (int, error)
```

迭代中途 `break`/出错即停止消费，`Close` 关底层连接；`err == io.EOF` 或 seq 正常结束表示取尽。

### 5.3 WADO-URI

```go
type URIRequest struct {
    StudyUID, SeriesUID, ObjectUID string // 必填

    ContentType      string   // "" = application/dicom（DICOM 实例）；填 image/* 走渲染事务
    TransferSyntax   string   // 仅 DICOM 实例
    Charset          string
    Anonymize        bool
    Annotation       []string // "patient","technique"
    FrameNumber      int      // 渲染
    ImageQuality     int      // 渲染 1-100
    Rows, Columns    int      // 成对
    Region           *[4]float64
    WindowCenter, WindowWidth *float64 // 成对，与 Presentation* 互斥
    PresentationUID, PresentationSeriesUID string // 成对
    Extra            url.Values // 透传
}
func (c *Client) RetrieveURIInstance(ctx context.Context, req URIRequest) (*URIResponse, error)

type URIResponse struct {
    ContentType string        // "application/dicom" 或 image/*
    Body        io.ReadCloser // 单段流
}
```

客户端先做本地校验（必填、成对约束、互斥约束——标准规定服务器必返 400 的情形提前拦截），再发请求。

### 5.4 多目标复用：Fork 与 multi.Registry

单个 Client 的 BaseURL 在构造时绑定（不可变，线程安全）。跨医院/租户/业务线复用有两个层次：

1. **核心库到 `Fork` 为止**（见 D1）：共享底座装配、仅换 BaseURL。核心库对"有多少个目标、按什么维度区分目标"零假设——这些是业务概念。
2. **`multi` 子包（纯便利，可不依赖）**：一个**泛型**的"业务键 → Client"缓存，把"键是什么"同样留给业务层：

```go
package multi

// Endpoint：一个目标的接入点 = 标准基地址（不含任何服务路由）+ 每个服务各自独立的路由。
// 两条路由互不挂靠，库只负责把 Base 与路由拼成完整 URL；空路由 = 未提供该服务。
type Endpoint struct {
    Base     string        // 如 "https://gw.example.com"（scheme://host[/静态前缀]，不含服务路由）；必填
    RSRoute  string        // 如 "/api/wado/H0001/RIS/wado-rs"；空 = 无 WADO-RS，Gateway.RS 为 nil
    URIRoute string        // 如 "/api/wado/H0001/RIS/wado-uri"；空 = 无 WADO-URI，Gateway.URI 为 nil
    Options  []wado.Option // 该目标专属装配（鉴权/超时等），叠加在 Defaults 之上
}

// Resolver：业务键 → 接入点。K 的语义与映射逻辑完全是业务层的知识。
type Resolver[K comparable] interface {
    Resolve(ctx context.Context, key K) (Endpoint, error)
}
type ResolverFunc[K comparable] func(ctx context.Context, key K) (Endpoint, error) // 函数适配

func NewRegistry[K comparable](resolver Resolver[K], defaults ...wado.Option) *Registry[K]
func (r *Registry[K]) Client(ctx context.Context, key K) (*Gateway, error)
    // Resolve → 校验 → 底座.Fork(...) → 缓存；键只在此处出现一次
func (r *Registry[K]) Invalidate(key K) // 前缀变更/凭证轮换时失效重建

// —— 现成的 Resolver 实现（均为便利品，可换自定义）——
multi.Static[K](map[K]Endpoint{...}) // 静态表：配置文件/DB 加载，最常用
multi.Template(base, multi.Routes{RS: "/api/wado/{h}/{b}/wado-rs", URI: "/api/wado/{h}/{b}/wado-uri"}, vars)
                                     // 模板：两条路由各自独立填充占位符（vars 提供值并转义）
```

要点：

- **库（含 multi）对两件事零假设**：① 前缀的内部结构（D1）；② 区分目标的**键**的语义。`K` 只是 `comparable`——实例化成 `string`（医院编码/租户 ID/业务编码）、`int`、或 `struct{Tenant, Biz string}` 组合键都可以，编译期类型安全。
- `Template` 是便利品：`Routes` 的两条路由模板各自独立、支持任意命名占位符，`vars(K)` 提供值并转义后拼到 Base 上；不合适（查库/配置中心/带签名）就自定义 `ResolverFunc` 返回 `Endpoint`。两个服务在不同主机时 `multi` 不适用——直接用 `wadors.New`/`wadouri.New` 自行管理。
- Registry 内部只创建**一个**底座 Client（承载 Defaults 与共享 `*http.Client`），所有实例由 `Fork` 派生——整个进程对同一网关只有一个连接池。
- 若"按键缓存"这个形状本身都不合适（目标集合每请求动态决定等），业务层直接用 `Fork` 自管理即可，`multi` 可以整个不要。

### 5.5 使用示例（前缀结构不同的多目标）

```go
ctx := context.Background()

// 场景一：标准基地址 + 两条独立的服务路由模板，占位符语义是业务侧数据；
// 两个业务参数自然构成一个组合键 K。
type routeKey struct{ Hospital, Biz string }

reg := multi.NewRegistry(
    multi.Template(
        "https://gw.example.com", // 标准基地址（不含服务路由）
        multi.Routes{
            RS:  "/api/wado/{hospitalCode}/{businessCode}/wado-rs",
            URI: "/api/wado/{hospitalCode}/{businessCode}/wado-uri",
        },
        func(k routeKey) map[string]string {
            return map[string]string{"hospitalCode": k.Hospital, "businessCode": k.Biz}
        },
    ),
    wado.WithTLSClientConfig(privateCA),
)

// hospitalCode / businessCode 只在这里出现一次；命中缓存，无网络开销
g, _ := reg.Client(ctx, routeKey{Hospital: "H0001", Biz: "RIS"})

// 之后所有方法只传 DICOM UID，不再出现任何业务参数：
//   g.RS.RetrieveStudy(...) → GET https://gw.example.com/api/wado/H0001/RIS/wado-rs/studies/{study}/...
//   g.URI.Retrieve(...)     → GET https://gw.example.com/api/wado/H0001/RIS/wado-uri?requestType=WADO&studyUID=...
// 厂家改路由（段改名/加段/换位置）→ 只改 Routes 模板字符串；映射入配置则只改配置 + Invalidate


// 场景二：不同医院是完全独立的服务器（各自网关，直接 New 或各建 Registry）
cc, _ := wadors.New("https://10.20.30.40:8443/dicomweb", wado.WithTLSClientConfig(privateCA))

// 整 Study 流式落盘
mp, err := g.RS.RetrieveStudy(ctx, "1.2.840.113619.2.1.1.1")
if err != nil { ... } // *StatusError 可判 IsNotFound()
defer mp.Close()
files, err := mp.WriteToDir("D:/cache/H0001/1.2.840...")

// 元数据（go-dicom dataset）
dss, _ := g.RS.StudyMetadata(ctx, studyUID)
for _, ds := range dss {
    sop, _ := ds.GetString(tag.SOPInstanceUID)
}

// 渲染图
img, _ := g.RS.RetrieveRenderedInstance(ctx, studyUID, seriesUID, sopUID,
    wadors.WithRenderedFormat("image/png"), wadors.WithWindow(40, 400))
defer img.Close()

// WADO-URI（老网关）
res, _ := g.URI.Retrieve(ctx, wadouri.Request{
    StudyUID: studyUID, SeriesUID: seriesUID, ObjectUID: sopUID,
    ContentType: "application/dicom",
})
```

## 6. 测试策略

1. **单元测试**
   - URL 矩阵：不同前缀（含/不含尾斜杠、多级前缀、带端口、IP）× 不同资源，断言最终 URL。
   - UID 校验表驱动（含注入向量 `../`、`%2e`、空组件、超长）。
   - WADO-URI 参数编码与本地成对/互斥校验。
   - multipart 解析：自造报文（多 part、gzip、chunked、part 头缺失、畸形 boundary）。
   - BulkDataURI 三种形式解析。
2. **集成测试（httptest 模拟 PACS）**：`internal/testutil` 提供可编程假服务器（按 UID 生成小 DICOM 文件、dicom+json、帧、渲染 PNG、错误码注入），全链路断言。
3. **真机兼容性（可选，`//go:build conformance`）**：对 Orthanc（DICOMweb 插件）与 dcm4chee-arc 的 docker 实例跑冒烟，作为兼容性回归靶机。

## 7. 实施里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| M1 | Client 装配、URL 策略、UID 校验、WADO-RS Study/Series/Instance + multipart 流式、错误模型 | 单测 + httptest 全链路；大响应内存占用恒定 |
| M2 | Metadata + dicomjson + BulkDataURI 解析 | 元数据/批量数据用例 |
| M3 | Frames、Rendered（含参数兼容与透传） | 渲染参数矩阵测试 |
| M4 | WADO-URI 全量参数与本地校验 | 参数编码快照测试 |
| M5 | 重试、日志/追踪插拔、multi 泛型注册表、真机兼容冒烟 | README + 示例 |

后续可自然扩展：QIDO-RS（查询）、STOW-RS（上传）复用同一 Client/传输层。

## 8. 开放问题（不阻塞 M1 开工，按默认值推进）

1. **鉴权方式**：医院侧是 Basic / OAuth2 Bearer / 私有签名头？→ 已全部预留插拔点，默认 Basic。
2. **渲染图/缩略图/MPR 是否一期就要**：默认一期只做 rendered，thumbnail/MPR/3D 挂二期。
3. **主要目标服务器清单**（决定参数命名兼容与靶机）：Orthanc / dcm4chee / 厂商网关？默认按经典命名 + 透传兜底。
4. **是否需要解析 DICOM 文件内容**（如读 Tag）：默认不解析（透传字节 + dicom+json），如需再引入 `suyashkumar/dicom` 做可选子包。
5. **模块路径**：`github.com/<org>/go-wado-client`，待确认 org。
6. **接入点（前缀）的来源与更新方式**：配置文件 / DB / 配置中心？是否需要热更新？默认 `Static` Resolver + `Invalidate()` 手动失效。

---

## 9. 实现状态与相对初稿的设计变更

M1–M4 + M5 核心已全部实现，46 个单元/httptest 测试全绿（`go vet` / `gofmt` 干净）。
实现过程中按评审意见做了如下调整（均已在代码与 README 落实）：

1. **双客户端拆分**：WADO-RS 与 WADO-URI 是两个标准，拆为 `wadors.Client` 与
   `wadouri.Client` 两个独立客户端；共享底座收敛到根包 `wado.Core`
   （Option 装配、鉴权、重试、日志、UID 校验、URL 工具），`Fork` 派生共享连接池。
2. **Module 路径**：`github.com/cocosip/go-wado-client`（仓库 git@github.com:cocosip/go-wado-client.git）。
3. **日志**：接入 `log/slog`，但**从不回退 `slog.Default()`**——`WithLogger(*slog.Logger)`
   / `WithLogHandler(slog.Handler)` 显式注入，缺省丢弃（`wado.DiscardLogger`）。
4. **DICOM 处理全部委托 go-dicom**（cocosip 同组织库），不自研：
   - 删除初稿的 `dicomjson` 自研模型；元数据解析走 `serialization.FromJSON`，
     返回 `*dataset.Dataset`；WADO 特有的相对 BulkDataURI 改写在 JSON 层完成后交给 FromJSON。
   - 新增可选子包 `dicomx`：multipart→parser 流式桥接（`Datasets`）+
     go-dicom-codecs 全量编解码器空导入注册（RLE/JPEG/JPEG-LS/JPEG2000/HTJ2K）。
   - 核心依赖策略由"零第三方依赖"调整为"标准库 + 组织内 go-dicom"。
5. **代码注释全英文**；测试覆盖：URL 矩阵、UID 注入向量、multipart 迭代/落盘/单段容错、
   元数据（含 multipart 包裹与 BulkDataURI 三形式）、渲染参数经典/新版/透传、
   WADO-URI 参数编码与成对/互斥校验、multi 注册表缓存/失效/模板、dicomx writer→parser 往返。

仍按初稿挂起的项：thumbnail / pixeldata / renderedmpr / rendered3d 等新版可选资源（D6 透传可兜底）、
真机兼容冒烟（Orthanc / dcm4chee）。

6. **代码走查修复（2026-09）**：
   - 实例元数据兼容单元素数组形式（Orthanc / dcm4che / dicomweb-client 的主流行为），多元素数组给出明确报错；
   - WADO-URI 必填 UID 三元组本地校验（原实现漏掉"必填"，与 §5.3 不符）；
   - Retry-After 以 MaxBackoff 封顶，RetryPolicy 落实初稿的 Jitter 字段；
   - 元数据 BulkDataURI 改写增加无键快路径（子串探测跳过 decode/walk/re-encode）；
   - multi.Registry 锁外 Resolve + double-check 回填，慢 Resolver 不再阻塞其他租户的缓存命中；
   - WriteToDir 对重复 Content-Location 回退序号命名、复制失败清理截断文件；
   - 杂项：rendered quality 1..100 本地校验、window 数值去指数格式化、
     `UIDError.Reason` 导出、`application/dicom` 大小写归一、`Core.Do` 的 body 重放约束文档化。

7. **代码走查修复（2026-09 第二轮，逐条对照现行标准文本核查）**：
   - **协议一致性**：
     - `transfer-syntax` 参数放行标准通配符 `*`（WADO-RS Accept 与 WADO-URI 查询参数两处，原先被 UID 白名单误拒）；
     - modern 模式 `window` 参数补齐为 `center,width,function` 三值（§8.3.5 要求三值必填，缺省 linear），新增 `WithWindowFunction`（linear/linear-exact/sigmoid，本地校验），classic 模式无此概念不受影响；
     - metadata 请求不再附带 `transfer-syntax`（dicom+json 无传输语法，原实现属非标参数）；
     - WADO-URI 渲染专用参数（frameNumber/imageQuality/rows/columns/region）与 `application/dicom` 组合时本地拒绝（§9.5：仅渲染事务有效）；`FrameNumber` 0 值语义（未设置）文档化。
   - **性能/保真**：
     - 元数据 BulkDataURI 改写改用 `json.Decoder.UseNumber()`，数字字面量跨 decode/re-encode 往返保持逐位保真；
     - 元数据改为逐 part 解析（数组 part 贡献元素、裸对象 part 贡献自身），修复"每 part 一数据集"的老服务器产出非法拼接 JSON 的问题；
     - `partFilename` 剥离 Content-Location 的 query/fragment，带参 URI 仍按 UID 命名落盘。
   - **代码质量**：
     - `StatusError`/`UIDError` 报文截断改为 UTF-8 安全（不切碎多字节字符，非法序列替换为 U+FFFD）；
     - `StatusError.Header` 脱敏 `Set-Cookie`（错误值会进入日志/缺陷报告）；
     - rendered 响应 `Header` 统一 Clone（与 wadouri.Response 一致）；
     - `multi.ErrUnknownKey` 改用 `errors.New`；`DiscardLogger` 改用 `slog.DiscardHandler`（Go 1.24+）；
     - `WithLenientUID` 注明路径穿越风险；`wadouri.New` 注明端点 URL 的 query/fragment 会被丢弃；
     - dicomx 包注释统一为英文（兑现 §9.5 的注释语言约定）。
