import{importShared as De}from"./__federation_fn_import-T4.js";import{h as Hx,r as Rm}from"./util-T3.js";import{m as Nm}from"./names-T5.js";
const{create:Mk}=await De("zustand");
document.title="Designer";
var Pre="0";var Pre=Pre+"_";
window.macPrefix=Pre;
class Rg{constructor(){this.devices={},this.linked={}}addNewDevice(e){this.devices[e.macUniq]=e}parseAndAddGroupsFromDev(e){}updateConditionsArrByMacUniq(e,t){this.devices[e].bytes=t}updateLinkedCondArrByMacUniq(e,t){this.linked[e]=t}clearLinkedCondArrByMacUniq(e){this.linked[e]=[]}getLinkedCondArr(e){return this.linked[e]}}
Rg.prototype.size=function(){return Object.keys(this.devices).length};
let y2;
const rN=()=>{y2=new Rg},
  st=Mk(()=>({ready:!1})),
  Js=()=>{rN(),st.setState({ready:!0})},
  pE=(e,t)=>{if(!st.getState().ready)throw new Error("not started");y2.clearLinkedCondArrByMacUniq(e);const n=[];for(const a of t){if(a.value>200)throw new Error("value out of range");a.link===void 0?n.push(a.value):(n.push(201,a.value),y2.linked[e].push(a.link))}return n.push(255),n},
  vD=(e,t)=>{if(!Nm.has(e))throw new Error("unknown module "+e);const n=y2.getLinkedCondArr(e)??[],a=[];let r=0;for(let o=0;o<t.length;o++)t[o]===201?a.push({value:t[++o],link:n[r++]}):a.push({value:t[o]});return a.map((o,i)=>({...o,uuid:"u"+i,macUniq:e,ordinalNo:i,hash:"h"+o.value,originalCond:"",legacyHash:""}))},
  wD=e=>{const t=window.macPrefix+Hx(e.macProd);y2.addNewDevice({macUniq:t}),Rm(t,e.macProd),y2.parseAndAddGroupsFromDev(e)},
  useLogic=Mk(e=>({save:(t,n)=>{const a=pE(t,n);y2.updateConditionsArrByMacUniq(t,a),e({saved:t});return vD(t,a)}})),
  boot=e=>{e().append("app"),Js()};
addEventListener("load",()=>Js());
export{boot};
