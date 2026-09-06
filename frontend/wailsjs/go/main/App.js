// Wails v2 生成的绑定 shim（wails build 会自动重新生成；手写版保证目录完整）
/* eslint-disable */
export function CreateRoom(mcPort) {
  return window['go']['main']['App']['CreateRoom'](mcPort);
}
export function JoinRoom(code) {
  return window['go']['main']['App']['JoinRoom'](code);
}
export function Leave() {
  return window['go']['main']['App']['Leave']();
}
export function Status() {
  return window['go']['main']['App']['Status']();
}
export function DetectMCPort() {
  return window['go']['main']['App']['DetectMCPort']();
}
export function StartLogWatch() {
  return window['go']['main']['App']['StartLogWatch']();
}
export function SetAdvanced(turn, mcDir, mailboxBase) {
  return window['go']['main']['App']['SetAdvanced'](turn, mcDir, mailboxBase);
}
export function GetAdvanced() {
  return window['go']['main']['App']['GetAdvanced']();
}
export function NetProbe() {
  return window['go']['main']['App']['NetProbe']();
}
