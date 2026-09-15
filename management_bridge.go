package main

const embeddedCSP = "default-src 'none'; connect-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"
const standaloneCSP = "default-src 'none'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

// The sandboxed srcdoc UI receives a MessagePort, never a management key.
// Only its embedding management page can establish this channel.
const codexWakeupBridgeJS = `
var embedded=window.parent!==window,hostPort=null,bridgeReady=false,requestID=0,pending=new Map();
function connectToManagement(){
  return new Promise(function(resolve,reject){
    var timeout=setTimeout(function(){window.removeEventListener("message",receive);reject(new Error("未连接管理中心，请安装配套 management.html 后重新打开"));},10000);
    function receive(event){
      if(event.source!==window.parent||!/^https?:\/\//.test(event.origin)||!event.data||event.data.type!=="codex-wakeup:connect"||event.data.version!==1||event.ports.length!==1)return;
      clearTimeout(timeout);window.removeEventListener("message",receive);
      hostPort=event.ports[0];
      hostPort.onmessage=function(event){
        var reply=event.data;
        if(!reply||!Number.isSafeInteger(reply.id)||!pending.has(reply.id))return;
        var item=pending.get(reply.id);pending.delete(reply.id);clearTimeout(item.timeout);
        if(reply.ok===true)item.resolve(reply.result);else item.reject(new Error(typeof reply.error==="string"?reply.error:"管理请求失败"));
      };
      hostPort.start();bridgeReady=true;resolve();
    }
    window.addEventListener("message",receive);
    window.parent.postMessage({type:"codex-wakeup:ready",version:1},"*");
  });
}
function authHeaders(){var key=q("key").value.trim();if(!key)throw new Error("请先输入 Management Key");return {"Authorization":"Bearer "+key};}
async function api(path,options){
  options=options||{};
  if(!embedded){
    options.headers=Object.assign({"Accept":"application/json"},options.headers||{},authHeaders());
    var response=await fetch("/v0/management/codex-wakeup/"+path,options);
    var result=await response.json().catch(function(){return {};});
    if(!response.ok)throw new Error(result.error&&result.error.message?result.error.message:"HTTP "+response.status);
    return result;
  }
  if(!bridgeReady)return Promise.reject(new Error("请从已登录的管理中心打开插件"));
  var method=options.method||"GET",id=++requestID;
  return new Promise(function(resolve,reject){
    var timeout=setTimeout(function(){pending.delete(id);reject(new Error("管理请求超时，请检查执行历史后再决定是否重试"));},120000);
    pending.set(id,{resolve:resolve,reject:reject,timeout:timeout});
    hostPort.postMessage({id:id,path:path,method:method,body:options.body});
  });
}
window.addEventListener("pagehide",function(){
  bridgeReady=false;if(hostPort)hostPort.close();
  pending.forEach(function(item){clearTimeout(item.timeout);item.reject(new Error("管理页面已关闭"));});pending.clear();
});
`
