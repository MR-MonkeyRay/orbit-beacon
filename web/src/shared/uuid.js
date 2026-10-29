export const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Detail links previously accepted only RFC UUID versions and variants.
export const isDetailUUID = (value) => uuid.test(value)
  && '12345678'.includes(value[14]?.toLowerCase())
  && '89ab'.includes(value[19]?.toLowerCase());
