// 匹配 KEY=VALUE，VALUE 可能是 "带引号,可含逗号" 或裸值
const ATTR_RE = /([A-Za-z0-9][A-Za-z0-9-]*)=("(?:[^"\\]|\\.)*"|[^,]*)/g;

/** 简单的类型推断：纯整数/小数转 Number，其余保持字符串 */
function coerce(v) {
  if (v === '') return '';
  if (/^-?\d+$/.test(v)) return Number(v);
  if (/^-?(?:\d+\.\d+|\d+)$/.test(v)) return Number(v);
  return v;
}

/** 解析 "#EXT-X-XXX:KEY=VAL,KEY2="VAL2"" 里的属性串 */
function parseAttributes(input) {
  const out = {};
  if (!input) return out;
  ATTR_RE.lastIndex = 0;
  let m;
  while ((m = ATTR_RE.exec(input)) !== null) {
    let val = m[2];
    if (val.length >= 2 && val.startsWith('"') && val.endsWith('"')) {
      val = val.slice(1, -1).replace(/\\"/g, '"');
    }
    out[m[1]] = coerce(val);
  }
  return out;
}

/* ------------------------------------------------------------------ *
 * 辅助解析
 * ------------------------------------------------------------------ */

function parseResolution(res) {
  if (typeof res !== 'string') return null;
  const m = /^(\d+)x(\d+)$/.exec(res);
  return m ? { width: Number(m[1]), height: Number(m[2]) } : null;
}

function parseCodecs(codecs) {
  if (typeof codecs !== 'string') return [];
  return codecs.split(',').map((s) => s.trim()).filter(Boolean);
}

function parseFrameRate(fr) {
  if (fr === undefined || fr === null) return null;
  const n = Number(fr);
  return Number.isFinite(n) ? n : null;
}

/* ------------------------------------------------------------------ *
 * 单个变体（清晰度）构建
 * ------------------------------------------------------------------ */

function buildVariant(attrs, uri) {
  const variant = {
    uri: uri || null,
    bandwidth: typeof attrs.BANDWIDTH === 'number' ? attrs.BANDWIDTH : null,
    averageBandwidth:
      typeof attrs['AVERAGE-BANDWIDTH'] === 'number' ? attrs['AVERAGE-BANDWIDTH'] : null,
    resolution: parseResolution(attrs.RESOLUTION),
    codecs: parseCodecs(attrs.CODECS),
    frameRate: parseFrameRate(attrs['FRAME-RATE']),
    hdcpLevel: attrs['HDCP-LEVEL'] || null,
    pathwayId: attrs['PATHWAY-ID'] || null,
    audio: attrs.AUDIO || null,
    video: attrs.VIDEO || null,
    subtitles: attrs.SUBTITLES || null,
    closedCaptions: attrs['CLOSED-CAPTIONS'] || null,
  };

  // B 站私有字段（有则带上，方便下游直接取用）
  if ('BILI-QN' in attrs) variant.biliQn = attrs['BILI-QN'];
  if ('BILI-DISPLAY' in attrs) variant.biliDisplay = attrs['BILI-DISPLAY'];
  if ('BILI-ORDER' in attrs) variant.biliOrder = attrs['BILI-ORDER'];
  if ('BILI-STREAM' in attrs) variant.biliStream = attrs['BILI-STREAM'];

  variant.attributes = attrs; // 原始属性，保底
  return variant;
}

/* ------------------------------------------------------------------ *
 * 主解析函数
 * ------------------------------------------------------------------ */

function parseM3U8(text) {
  if (typeof text !== 'string') throw new TypeError('parseM3U8(text) 需要字符串');

  text = text.replace(/^\uFEFF/, ''); // 去 BOM
  const lines = text.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);

  if (!lines.length || !lines[0].startsWith('#EXTM3U')) {
    throw new Error('不是合法的 M3U8：缺少 #EXTM3U 头');
  }

  const result = {
    isMaster: false,
    version: null,
    allowCache: null,
    independentSegments: false,
    contentSteering: null,
    variants: [],        // EXT-X-STREAM-INF
    iFrameVariants: [],  // EXT-X-I-FRAME-STREAM-INF
    media: [],           // EXT-X-MEDIA
    segments: [],        // 媒体播放列表中的分片
    unknownTags: [],
  };

  let pendingStream = null;  // 等待下一行 URI 的 EXT-X-STREAM-INF

  for (let i = 1; i < lines.length; i++) {
    const line = lines[i];

    /* ---------- 非注释行：URI ---------- */
    if (!line.startsWith('#')) {
      if (pendingStream) {
        pendingStream.uri = line;
        result.variants.push(pendingStream);
        pendingStream = null;
      } else if (!result.isMaster) {
        result.segments.push({ uri: line });
      }
      continue;
    }

    /* ---------- 注释行：解析标签 ---------- */
    const colon = line.indexOf(':');
    const tag = colon === -1 ? line : line.slice(0, colon);
    const value = colon === -1 ? '' : line.slice(colon + 1);

    switch (tag) {
      case '#EXT-X-VERSION':
        result.version = coerce(value);
        break;

      case '#EXT-X-ALLOW-CACHE':
        result.allowCache = value.toUpperCase() === 'YES';
        break;

      case '#EXT-X-INDEPENDENT-SEGMENTS':
        result.independentSegments = true;
        break;

      case '#EXT-X-CONTENT-STEERING':
        result.contentSteering = parseAttributes(value);
        result.isMaster = true;
        break;

      case '#EXT-X-STREAM-INF': {
        // 上一个还没配对的（异常情况）先收下
        if (pendingStream) result.variants.push(pendingStream);
        result.isMaster = true;
        pendingStream = buildVariant(parseAttributes(value), null);
        break;
      }

      case '#EXT-X-I-FRAME-STREAM-INF': {
        result.isMaster = true;
        const attrs = parseAttributes(value);
        const uri = attrs.URI || null;
        result.iFrameVariants.push(buildVariant(attrs, uri));
        break;
      }

      case '#EXT-X-MEDIA':
        result.isMaster = true;
        result.media.push(parseAttributes(value));
        break;

      // 媒体播放列表标签（简单收集，不做深度解析）
      case '#EXT-X-TARGETDURATION':
      case '#EXT-X-MEDIA-SEQUENCE':
      case '#EXT-X-PLAYLIST-TYPE':
      case '#EXTINF':
      case '#EXT-X-ENDLIST':
        break;

      default:
        if (!result.unknownTags.includes(tag)) result.unknownTags.push(tag);
        break;
    }
  }

  // 收尾：最后一条没有跟 URI 的 STREAM-INF
  if (pendingStream) result.variants.push(pendingStream);

  return result;
}

function onFetchStreamUrl(data) {
  try {
    // 1. 请求 B 站 master m3u8 接口
    var apiUrl = 'https://api.live.bilibili.com/xlive/play-gateway/master/url?cid=' + data.roomid;
    var res = fetchSync(apiUrl, {
      method: 'GET'
    });

    if (!res || !res.ok) {
      console.error('获取 m3u8 失败，HTTP 状态：' + (res ? res.status : '无响应'));
      return null; // 返回 null 让录播姬走默认逻辑
    }

    var m3u8 = res.body;

    // 2. 解析 master m3u8，找出 BANDWIDTH 最大的 #EXT-X-STREAM-INF 对应的 URI
    var m3u8obj = parseM3U8(m3u8);
    var best = null;
    for (var i = 0; i < m3u8obj.variants.length; i++) {
      var variant = m3u8obj.variants[i];
      if (!best || (variant.bandwidth || 0) > (best.bandwidth || 0)) {
        best = variant;
      }
    }

    if (!best || !best.uri) {
      console.error('m3u8 中未找到可用的 STREAM-INF 流地址');
      return null;
    }

    // 3. 拼成本地代理地址
    var proxyUrl = 'http://127.0.0.1:8080/index.flv?url=' + encodeURIComponent(best.uri);
    console.log('选用码率 ' + best.bandwidth + 'Qn' + best.biliQn);
    return proxyUrl;

  } catch (e) {
    console.error('onFetchStreamUrl 执行出错：' + e);
    return null;
  }
}