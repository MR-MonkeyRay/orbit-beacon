export const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// 详情链接此前只接受 RFC 规定的 UUID 版本和变体。
export const isDetailUUID = (value) => uuid.test(value)
  && '12345678'.includes(value[14]?.toLowerCase())
  && '89ab'.includes(value[19]?.toLowerCase());
