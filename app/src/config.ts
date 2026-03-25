// @ts-ignore
const isDev = typeof __DEV__ !== 'undefined' ? __DEV__ : true

export const API_BASE_URL = isDev
  ? 'http://localhost:8081/api/v1'
  : 'https://your-server.com/api/v1'

export const WS_BASE_URL = API_BASE_URL
  .replace('https://', 'wss://')
  .replace('http://', 'ws://')
  .replace('/api/v1', '/api/v1/ws')
