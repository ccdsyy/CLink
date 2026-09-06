// CLink 信箱协议端到端实测：模拟 UAPI Clipzy 完整流程
// 运行：node tests/mailbox-test.js
const LZString = require('./node_modules/lz-string');
const crypto = require('crypto');

const BASE = 'https://paste.sdjz.wiki/api';

// ---- 1. 生成 AES-GCM-256 密钥（与 Clipzy 官方 JS 示例等价）----
const keyBytes = crypto.randomBytes(32);            // AES-256
const keyB64 = keyBytes.toString('base64');          // 分享给对方的密钥

// ---- 2. 压缩 + 加密（官方示例：LZString.compressToUTF16 -> AES-GCM(iv=12) -> base64(iv||ct)）----
const originalText = JSON.stringify({ proto: 'clink/0.1', type: 'room-offer', roomCode: 'A1B2C3', ts: Date.now() });
const compressed = LZString.compressToUTF16(originalText);
const iv = crypto.randomBytes(12);
const cipher = crypto.createCipheriv('aes-256-gcm', keyBytes, iv);
const ct = Buffer.concat([cipher.update(Buffer.from(compressed, 'utf16le')), cipher.final()]);
const authTag = cipher.getAuthTag();                 // GCM 16字节认证标签
const dataToSend = Buffer.concat([iv, ct, authTag]).toString('base64');

// ---- 3. 上传到信箱 ----
async function main() {
  console.log('原始明文 :', originalText);
  console.log('密钥B64  :', keyB64);

  const storeResp = await fetch(`${BASE}/store`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ compressedData: dataToSend, ttl: 600 })
  });
  const storeJson = await storeResp.json().catch(() => ({}));
  console.log('STORE 状态:', storeResp.status, '响应:', JSON.stringify(storeJson));
  if (!storeJson.id) { console.log('上传失败，停止后续测试'); return; }

  // ---- 4. 取回并解密（GET /get?id=）----
  const getResp = await fetch(`${BASE}/get?id=${encodeURIComponent(storeJson.id)}`);
  const getJson = await getResp.json().catch(() => ({}));
  console.log('GET   状态:', getResp.status, '含密文:', !!getJson.compressedData);

  try {
    const raw = Buffer.from(getJson.compressedData, 'base64');
    const rIv = raw.subarray(0, 12);
    const rTag = raw.subarray(raw.length - 16);
    const rCt = raw.subarray(12, raw.length - 16);
    const decipher = crypto.createDecipheriv('aes-256-gcm', keyBytes, rIv);
    decipher.setAuthTag(rTag);
    const plainBuf = Buffer.concat([decipher.update(rCt), decipher.final()]);
    const decompressed = LZString.decompressFromUTF16(plainBuf.toString('utf16le'));
    console.log('解密结果 :', decompressed);
    console.log(decompressed === originalText ? '[OK] 端到端信箱链路验证成功' : '[FAIL] 内容不一致');
  } catch (e) {
    console.log('[FAIL] 解密失败（IV/Tag 拼接顺序需对照官方示例调整）:', e.message);
  }

  // ---- 5. 验证方法二：raw 接口（服务器端解密）----
  try {
    const rawResp = await fetch(`${BASE}/raw/${storeJson.id}?key=${encodeURIComponent(keyB64)}`);
    const rawText = await rawResp.text();
    console.log('RAW   状态:', rawResp.status, '内容前120字:', rawText.slice(0, 120).replace(/\n/g, '\\n'));
    console.log(rawResp.ok && rawText.trim() === originalText ? '[OK] raw 服务器解密路径可用' : '[WARN] raw 路径格式不同（可只依赖 /get 端到端模式）');
  } catch (e) {
    console.log('[WARN] raw 路径测试异常:', e.message);
  }
}
main().catch(e => console.error('测试异常:', e.message));
