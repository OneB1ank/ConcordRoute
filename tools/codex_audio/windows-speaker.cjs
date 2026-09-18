"use strict";

// Windows 引擎输出本地 WAV，页面只播放音频；单段最多400字，逐段合成，不排无限播放队列。
class WindowsSpeaker {
  constructor({ fetch, connection, onError = () => {}, Audio = globalThis.Audio, urls = globalThis.URL }) {
    this.fetch=fetch; this.connection=connection; this.onError=onError; this.Audio=Audio; this.urls=urls;
  }
  speak(text) {
    if (typeof text !== "string" || !text.trim() || text.length > 20000) throw new Error("请选择或输入 1–20000 字的朗读内容");
    this.cancel();
    const controller=this.controller=new AbortController();
    this.work=this.play(text,controller.signal).catch(error=>{
      if (!controller.signal.aborted) this.onError(error);
    }).finally(()=>{ if(this.controller===controller)this.controller=null; });
    return "Windows 本机语音";
  }
  async play(text,signal) {
    // UTF-16 切片避免把代理对拆成两段。
    const chunks=[];let offset=0;
    while(offset<text.length) {
      let end=Math.min(offset+400,text.length);
      if(end<text.length&&/[\uD800-\uDBFF]/.test(text[end-1]))end--;
      chunks.push(text.slice(offset,end));offset=end;
    }
    for(const chunk of chunks) {
      signal.throwIfAborted();
      const config=await this.connection(signal);
      signal.throwIfAborted();
      const response=await this.fetch(config.url+"/audio/speech",{method:"POST",signal,
        headers:{"Content-Type":"application/json",Authorization:"Bearer "+config.capability},
        body:JSON.stringify({text:chunk}),credentials:"omit",redirect:"error"});
      if(!response.ok||!/^audio\/wav(?:;|$)/i.test(response.headers.get("content-type")||""))throw new Error("本机语音合成失败");
      const blob=await response.blob();signal.throwIfAborted();
      if(blob.size>8*1024*1024)throw new Error("本机语音音频超出上限");
      const url=this.urls.createObjectURL(blob), audio=new this.Audio(url);
      try {
        await new Promise((resolve,reject)=>{
          let timer;
          const cleanup=()=>{clearTimeout(timer);audio.onended=audio.onerror=null;signal.removeEventListener("abort",aborted);};
          const aborted=()=>{audio.pause();cleanup();reject(new Error("speech_cancelled"));};
          audio.onended=()=>{cleanup();resolve();};
          audio.onerror=()=>{cleanup();reject(new Error("本机音频播放失败"));};
          signal.addEventListener("abort",aborted,{once:true});
          timer=setTimeout(()=>{audio.pause();cleanup();reject(new Error("本机朗读播放超时"));},180000);
          if(signal.aborted){aborted();return;}
          Promise.resolve().then(()=>audio.play()).catch(error=>{cleanup();reject(error);});
        });
      } finally { audio.pause();audio.removeAttribute("src");audio.load();this.urls.revokeObjectURL(url); }
    }
  }
  cancel() { this.controller?.abort();this.controller=null; }
}
module.exports={WindowsSpeaker};
