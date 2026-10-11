# hls to flv 协议转换器

这是一个将 HLS 流转换为 FLV 流的工具。

不依赖ffmpeg，完全使用 Go 语言实现。

当用户请求 http://127.0.0.1:8080/http://example.com/stream.m3u8 时，工具会将指定的 HLS 流转换为 FLV 流并返回。

## 运行

```
go build -o blrechelper .
./blrechelper -addr 127.0.0.1:8080
```

有两种请求写法，任选其一。

### 推荐：查询参数写法

```
http://127.0.0.1:8080/index.flv?url=<urlencode 后的 m3u8 地址>
```

例如：

```
http://127.0.0.1:8080/index.flv?url=https%3A%2F%2Fexample.com%2Fstream.m3u8
```

上游地址经过百分号编码，因此**它自身带查询串时也能原样保留**，这是该写法
最大的好处：

```
http://127.0.0.1:8080/index.flv?url=https%3A%2F%2Fexample.com%2Flive.m3u8%3Ftoken%3Dabc%26sign%3Ddeadbeef
```

`url`、`u`、`src` 三个参数名都认，`url` 优先。

用一个 shell 命令生成：

```sh
# bash
curl -s "http://127.0.0.1:8080/index.flv?url=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=""))' "$M3U8")" -o out.flv
```

```powershell
# PowerShell
$u = [uri]::EscapeDataString($M3U8)
curl.exe -s "http://127.0.0.1:8080/index.flv?url=$u" -o out.flv
```

### 兼容：路径写法

```
http://127.0.0.1:8080/https://example.com/stream.m3u8
```

上游 URL 就是去掉首个 `/` 之后的完整地址。部分客户端会把 `//` 折叠成 `/`，
因此下面这种写法也支持：

```
http://127.0.0.1:8080/https:/example.com/stream.m3u8
```

注意路径写法在上游 URL 自带 `?`/`&` 时并不可靠（这些字符会被当成网关请求
自己的查询串），这种场景请用查询参数写法。

### 播放

```
ffplay "http://127.0.0.1:8080/index.flv?url=https%3A%2F%2Fexample.com%2Fstream.m3u8"
```

## 参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8080` | 监听地址 |
| `-user-agent` | 内置浏览器 UA | 请求上游时使用的 `User-Agent` |
| `-referer` | 上游根地址 | 请求上游时使用的 `Referer` |
| `-request-timeout` | `20s` | 单个上游请求的超时时间 |
| `-live` | `false` | 跳过直播列表里已缓冲的旧分片，尽量从最新处开始 |
| `-v` | `false` | 在 stderr 打印每个分片的进度 |

## 实现

| 包 | 职责 |
| --- | --- |
| `internal/m3u8` | 解析 HLS 播放列表（`EXTINF`、`EXT-X-MAP`、`EXT-X-BYTERANGE`、`EXT-X-KEY`、`EXT-X-MEDIA-SEQUENCE`、主列表变体等） |
| `internal/mp4` | 解析 fMP4/CMAF 分片：`moov` 初始化段与 `moof`/`trun`/`mdat` 媒体段 |
| `internal/flv` | 生成 FLV：文件头、AVC/HEVC 视频标签、AAC 音频标签、AMF0 `onMetaData` |
| `internal/hlsflv` | 串联整个流程：拉取播放列表、下载分片、重封装、直播列表重载 |

支持的编码：

* 视频 H.264（`avc1`/`avc3`）与 H.265（`hvc1`/`hev1`）
* 音频 AAC（`mp4a`）与 Opus

不支持的输入会返回明确错误，而不是产出损坏的流：带 CENC 加密的分片、
没有 `EXT-X-MAP` 的 MPEG-TS 播放列表、以及 FLV 无法承载的编码。

## HEVC：使用 codec 12

遇到 HEVC 时，**使用 codec ID 12** 承载，而不是 Enhanced RTMP 的 fourcc
形式。也就是说视频标签头是：

```
frameType(4) | codecID = 12     packetType = 0（序列头）/ 1（帧）
```

跟随其后的是长度前缀的 HEVC NAL 单元，序列头里放
`HEVCDecoderConfigurationRecord`（含 VPS/SPS/PPS），与 AVC 的布局一致。

这是刻意与 ffmpeg 上游不同的选择：ffmpeg 只在 Enhanced RTMP 形式下支持
HEVC-in-FLV（`isExHeader` 位 + `"hvc1"` fourcc），`flvdec.c` 里没有 codec ID
12 的分支。因此用 ffprobe/ffmpeg 直接探测该 FLV 时会把视频编码报成
`unknown`，这是预期现象，不代表流有问题。要验证 HEVC 负载本身是否合法，
可以把它抽成 Annex B 后用 `-f hevc` 解码：

```
ffmpeg -f hevc -i extracted.h265 -f null -
```

## 测试

```
go test ./...
```

单元测试只依赖内存里构造的 mp4/m3u8/FLV 数据，不需要网络。

针对真实 CDN 的集成测试默认跳过，用环境变量开启：

```
BLREC_TEST_HLS_AVC_URL=https://.../index.m3u8 \
BLREC_TEST_HLS_HEVC_URL=https://.../index.m3u8 \
go test ./internal/hlsflv/ -run TestLive -v
```

示例地址见 `example/stream.md`。

## 已知限制

* 只支持 fMP4/CMAF 分片（`EXT-X-MAP`）。MPEG-TS 分片尚未实现。
* 加密流（`EXT-X-KEY`）会以明确的错误跳过该分片，不做解密。
* 直播流没有时长，因此不会写入 `duration` 元数据字段；点播流也没有实现，
  因为播放列表里的总时长需要遍历全部分片。
