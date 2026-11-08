/* Standalone dashboard editors. Policy definitions and catalogs come from the backend. */
window.TeamRoutingEditors = (() => {
  'use strict';
  const copy=x=>JSON.parse(JSON.stringify(x));
  const node=(tag,text,cls)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;};
  const opt=(select,value,label)=>{const n=node('option',label);n.value=value;select.append(n);};
  const labeled=(title,input)=>{const wrap=node('div'),label=node('label',title);label.htmlFor=input.id;wrap.append(label,input);return wrap;};
  let serial=0;
  function targetEditor(saved,catalog,change,prefix) {
    const root=node('div',undefined,'route-grid target-editor'),provider=node('select'),model=node('select'),search=node('input');
    provider.id='provider-'+(++serial);model.id='model-'+serial;search.type='search';search.placeholder='Filter supported models';search.setAttribute('aria-label','Filter models for '+prefix);model.size=5;
    let selected=saved?.model||'',chosen=saved?.provider||'';
    const rebuild=()=>{model.replaceChildren();opt(model,'',provider.value?'Choose a model':'Choose a provider first');const all=catalog().models.filter(m=>m.providers.includes(provider.value)),visible=all.filter(m=>m.id.toLowerCase().includes(search.value.toLowerCase()));if(selected&&!visible.some(m=>m.id===selected))opt(model,selected,selected+(all.some(m=>m.id===selected)?' · selected':' · unavailable'));visible.forEach(m=>opt(model,m.id,m.id));model.value=selected;};
    const refresh=()=>{chosen=provider.value||chosen;provider.replaceChildren();opt(provider,'','Choose a provider');catalog().providers.forEach(p=>opt(provider,p.id,p.label+' ('+p.model_count+')'));if(chosen&&!catalog().providers.some(p=>p.id===chosen))opt(provider,chosen,chosen+' · unavailable');provider.value=chosen;rebuild();};
    provider.onchange=()=>{chosen=provider.value;if(!catalog().models.some(m=>m.id===selected&&m.providers.includes(chosen)))selected='';search.value='';rebuild();change();};model.onchange=()=>{selected=model.value;change();};search.oninput=rebuild;
    const models=labeled('Target model',model);models.prepend(search);root.append(labeled('Target provider',provider),models);refresh();
    return {root,refresh,enable:on=>{provider.disabled=model.disabled=search.disabled=!on;},read:()=>{if(!provider.value||!model.value)throw Error('Choose a provider and model for '+prefix+'.');return {provider:provider.value,model:model.value};}};
  }
  function clientEditor(client,route,rules,catalog,change) {
    const root=node('section',undefined,'card'),heading=node('div',undefined,'client-title');root.dataset.clientId=client.id;heading.append(node('h3',client.name),node('span',client.masked+' · '+client.fingerprint,'muted'));root.append(heading);
    const label=node('input');label.id='label-'+(++serial);label.placeholder=client.name;label.maxLength=80;label.oninput=change;root.append(labeled('Label',label));
    const mode=node('select');mode.id='default-'+serial;opt(mode,'requested','Use requested model');opt(mode,'override','Override unmatched requests');mode.value=route?'override':'requested';root.append(labeled('Default routing',mode));
    const fallback=targetEditor(route,catalog,change,client.name+' default');root.append(fallback.root);fallback.enable(mode.value==='override');mode.onchange=()=>{fallback.enable(mode.value==='override');change();};
    const details=node('details',undefined,'model-rule-editor'),list=node('div'),add=node('button','Add model rule');details.open=Object.keys(rules||{}).length>0;details.append(node('summary','Optional rules for specific requested models'),node('p','Exact model rules take precedence over the default above. Unmatched models keep the default. Targets are resolved once, without chained remapping.'));
    const rows=[];
    function addRule(source='',saved={action:'override'}) {
      const row=node('section',undefined,'model-rule-row'),sourceInput=node('input'),action=node('select'),remove=node('button','Remove rule');sourceInput.id='source-'+(++serial);sourceInput.value=source;sourceInput.placeholder='Exact requested model ID';sourceInput.maxLength=256;sourceInput.oninput=change;
      const sourceSelect=node('select'),sourceSearch=node('input');sourceSelect.id='source-select-'+serial;sourceSearch.type='search';sourceSearch.placeholder='Search available requested models';sourceSearch.setAttribute('aria-label','Filter requested models for '+client.name+' rule '+serial);
      const refreshSource=()=>{const selected=sourceInput.value.trim(),all=catalog().models,visible=all.filter(m=>(m.id+' '+m.providers.join(' ')).toLowerCase().includes(sourceSearch.value.toLowerCase()));sourceSelect.replaceChildren();opt(sourceSelect,'','Choose a requested model');if(selected&&!visible.some(m=>m.id===selected))opt(sourceSelect,selected,selected+(all.some(m=>m.id===selected)?' · selected':' · custom ID'));visible.forEach(m=>opt(sourceSelect,m.id,m.id));sourceSelect.value=selected;};
      sourceSelect.onchange=()=>{sourceInput.value=sourceSelect.value;change();};sourceSearch.oninput=refreshSource;sourceInput.oninput=()=>{refreshSource();change();};
      const sourceControl=labeled('Requested model',sourceSelect);sourceControl.prepend(sourceSearch);
      action.id='action-'+serial;opt(action,'override','Override this model');opt(action,'requested','Use this requested model');action.value=saved.action;
      const target=targetEditor(saved.action==='override'?saved:null,catalog,change,client.name+' model rule');target.enable(action.value==='override');action.onchange=()=>{target.enable(action.value==='override');change();};
      const refresh=()=>{refreshSource();target.refresh();};refresh();
      row.append(sourceControl,labeled('Requested model ID (optional)',sourceInput),labeled('Action',action),target.root,remove);list.append(row);
      const entry={row,sourceInput,action,target,refresh};rows.push(entry);remove.onclick=()=>{rows.splice(rows.indexOf(entry),1);row.remove();change();};
    }
    Object.entries(rules||{}).sort(([a],[b])=>a.localeCompare(b)).forEach(([source,r])=>addRule(source,r));add.onclick=()=>{addRule();details.open=true;change();};details.append(list,add);root.append(details);
    return {root,label,refreshCatalog:()=>{fallback.refresh();rows.forEach(r=>r.refresh());},read:()=>{const out={label:label.value.trim(),route:mode.value==='override'?fallback.read():null,rules:{}};for(const r of rows){const source=r.sourceInput.value.trim();if(!source)throw Error('Enter the exact requested model for '+client.name+'.');if(Object.hasOwn(out.rules,source))throw Error('Duplicate requested model '+source+' for '+client.name+'.');out.rules[source]=r.action.value==='requested'?{action:'requested'}:{action:'override',...r.target.read()};}return out;}};
  }
  function inputField(field,value,change,prefix) {
    const id='setting-'+(++serial),scale=field.format==='percent'?100:1;let input,read,raw;
    if(field.format==='weekdays'){
      input=node('div',undefined,'weekdays');['Sun','Mon','Tue','Wed','Thu','Fri','Sat'].forEach((day,i)=>{const label=node('label',day),check=node('input');check.type='checkbox';check.value=i;check.checked=(value||[]).includes(i);check.setAttribute('aria-label',prefix+' '+field.title+': '+day);check.onchange=change;label.prepend(check);input.append(label);});read=()=>[...input.querySelectorAll('input:checked')].map(n=>Number(n.value));raw=read;
    }else if(field.type==='boolean'){input=node('input');input.type='checkbox';input.checked=!!value;input.onchange=change;read=raw=()=>input.checked;
    }else if(field.enum){input=node('select');field.enum.forEach(v=>opt(input,v,field.enum_labels?.[v]||v));input.value=value||field.enum[0];input.onchange=change;read=raw=()=>input.value;
    }else{input=node('input');input.type=['integer','number'].includes(field.type)?'number':'text';input.value=typeof value==='number'?Number((value*scale).toFixed(6)):value??'';if(field.minimum!=null)input.min=field.minimum*scale;if(field.maximum!=null)input.max=field.maximum*scale;input.step=field.type==='integer'?'1':'0.1';input.oninput=change;raw=()=>input.value;read=()=>{if(input.type==='text')return input.value.trim();const n=Number(input.value);if(input.value===''||!Number.isFinite(n)||(field.minimum!=null&&n<field.minimum*scale)||(field.maximum!=null&&n>field.maximum*scale)||(field.type==='integer'&&!Number.isInteger(n)))throw Error('Check '+field.title+' for '+prefix+'.');return n/scale;};}
    input.id=id;input.setAttribute('aria-label',field.title+' for '+prefix);const wrap=labeled(field.title+(scale===100?' (%)':''),input);wrap.append(node('p',field.description,'help'));
    const restore=v=>{if(field.format==='weekdays'){input.querySelectorAll('input').forEach(n=>n.checked=(v||[]).includes(Number(n.value)));}else if(field.type==='boolean')input.checked=!!v;else input.value=v;};
    return {root:wrap,input,read,raw,restore,enable:on=>{if(field.format==='weekdays')input.querySelectorAll('input').forEach(n=>n.disabled=!on);else input.disabled=!on;}};
  }
  function subscriptionEditor(providerRoot,accountRoot,state,policy,spec,change,previous) {
    providerRoot.replaceChildren();accountRoot.replaceChildren();providerRoot.append(node('h2','Provider reserve policies'),node('p','Each provider has its own defaults and schedule. Only reported quota windows are used. Providers without a quota adapter keep native quota management.'));accountRoot.append(node('h2',spec.account_reserves.title),node('p',spec.account_reserves.description));
    const accounts=new Map(state.accounts.map(a=>[a.ID,a]));
    for(const id of [...Object.keys(policy.account_priority||{}),...Object.keys(policy.account_reserves||{}),...Object.keys(policy.account_ownership||{}),...Object.keys(policy.window_reserves||{}),...Object.keys(previous?.accounts||{})])if(!accounts.has(id))accounts.set(id,{ID:id,Name:id,Provider:'',disconnected:true,window_options:[]});
    const providers=new Map();for(const a of accounts.values())if(a.Provider){const p=providers.get(a.Provider)||{id:a.Provider,label:state.catalog.providers.find(p=>p.id===a.Provider)?.label||a.Provider,supported:false,five:false,weekly:false};p.supported ||= a.quota_supported;p.five ||= a.quota_features?.five_hour;p.weekly ||= a.quota_features?.weekly;providers.set(a.Provider,p);}
    for(const id of Object.keys(policy.provider_reserves||{}))if(!providers.has(id))providers.set(id,{id,label:id+' · disconnected',supported:false,five:false,weekly:false});
    const providerRows=new Map(),accountRows=new Map();
    for(const p of [...providers.values()].sort((a,b)=>a.label.localeCompare(b.label))){
      const saved=p.id==='claude'?policy.shared_claude:policy.provider_reserves?.[p.id],base=copy(saved||spec.provider_default);if(!saved){base.enabled=false;if(!p.five)base.five_hour_remaining_floor=0;if(!p.weekly)base.weekly_remaining_floor=0;}
      const card=node('details',undefined,'provider-policy');card.open=p.id==='claude'||!!base.enabled;card.append(node('summary',p.label),node('p',p.supported?'Quota-aware policy. Only windows reported for the effective model participate.':'No normalized quota adapter is available. Ownership, model routing and native cooldowns still work; this plugin cannot promise percentage reserves.','help'));
      const grid=node('div',undefined,'field-grid'),schedule=node('details'),fields=new Map();schedule.append(node('summary','Protection and weekly release schedule'));const scheduleGrid=node('div',undefined,'field-grid');let touched=false;
      for(const f of spec.provider_fields){const control=inputField(f,base[f.path],()=>{touched=true;change();},p.label);if(previous?.providers?.[p.id]?.values&&Object.hasOwn(previous.providers[p.id].values,f.path))control.restore(previous.providers[p.id].values[f.path]);fields.set(f.path,control);if(['enabled','five_hour_remaining_floor','weekly_remaining_floor'].includes(f.path))grid.append(control.root);else scheduleGrid.append(control.root);}
      touched=previous?.providers?.[p.id]?.touched||false;
      if(!p.supported&&!base.enabled)fields.get('enabled').enable(false);
      if(!p.five)fields.get('five_hour_remaining_floor').enable(false);if(!p.weekly)fields.get('weekly_remaining_floor').enable(false);
      schedule.append(scheduleGrid);card.append(grid,schedule);providerRoot.append(card);providerRows.set(p.id,{fields,base,saved,info:p,touched:()=>touched});
    }
    for(const a of [...accounts.values()].sort((a,b)=>(a.Provider+' '+(a.Email||a.Name||a.ID)).localeCompare(b.Provider+' '+(b.Email||b.Name||b.ID)))){
      const name=a.Email||a.Name||a.ID,card=node('section',undefined,'account-reserve-row');card.dataset.accountId=a.ID;card.append(node('h3',name),node('p',(a.Provider||'Disconnected credential')+' · '+(a.disconnected?'Saved rules are retained for this credential ID.':'Ownership and reserves are explicit per credential.'),'muted'));
      const own=node('select');own.id='owner-'+(++serial);opt(own,'inherit','Use default ('+(a.legacy_owned?'owned':'shared')+')');opt(own,'owned','Owned');opt(own,'shared','Shared');own.value=previous?.accounts?.[a.ID]?.own??(Object.hasOwn(policy.account_ownership||{},a.ID)?policy.account_ownership[a.ID]?'owned':'shared':'inherit');own.onchange=change;card.append(labeled('Ownership',own));
      const priority=inputField(spec.account_priority_field,policy.account_priority?.[a.ID]??0,change,name);if(previous?.accounts?.[a.ID]?.priority!=null)priority.restore(previous.accounts[a.ID].priority);card.append(priority.root);
      const pair=policy.account_reserves?.[a.ID],mode=node('select');mode.id='account-mode-'+serial;opt(mode,'default','Inherit provider defaults / ownership');opt(mode,'custom','Custom five-hour / weekly reserves');mode.value=previous?.accounts?.[a.ID]?.mode||(pair?'custom':'default');
      const grid=node('div',undefined,'route-grid'),inputs=new Map();grid.append(labeled('Reserve settings',mode));const provider=providerRows.get(a.Provider),base=provider?.base||spec.provider_default;
      for(const f of spec.account_reserves.fields){const control=inputField(f,pair?.[f.path]??(a.Owned?0:base[f.path]),change,name);if(previous?.accounts?.[a.ID]?.values&&Object.hasOwn(previous.accounts[a.ID].values,f.path))control.restore(previous.accounts[a.ID].values[f.path]);inputs.set(f.path,control);grid.append(control.root);}
      const update=()=>{for(const [path,c]of inputs)c.enable(mode.value==='custom'&&(path==='five_hour_remaining_floor'?a.quota_features?.five_hour:a.quota_features?.weekly));};update();mode.onchange=()=>{update();change();};if(!a.quota_supported&&!pair)mode.disabled=true;card.append(grid);
      const windows=node('details');windows.append(node('summary','Other native quota windows'));const windowRows=new Map(),choices=new Map((a.window_options||[]).filter(w=>!w.standard).map(w=>[w.key,w]));for(const [key,rule]of Object.entries(policy.window_reserves?.[a.ID]||{}))if(!choices.has(key))choices.set(key,{key,group:rule.group,window:rule.window,missing:true});
      for(const w of choices.values()){
        const rule=policy.window_reserves?.[a.ID]?.[w.key],row=node('div',undefined,'native-window-rule'),enable=node('input');enable.type='checkbox';enable.id='window-'+(++serial);enable.checked=previous?.accounts?.[a.ID]?.windows?.[w.key]?.enabled??!!rule;enable.onchange=change;row.append(labeled(w.group+' / '+w.window+(w.missing?' · no current reading':''),enable));const fields=new Map();
        for(const f of spec.window_fields){const control=inputField(f,rule?.[f.path]??0,change,name+' '+w.group+' '+w.window);const old=previous?.accounts?.[a.ID]?.windows?.[w.key]?.values;if(old&&Object.hasOwn(old,f.path))control.restore(old[f.path]);fields.set(f.path,control);row.append(control.root);}windows.append(row);windowRows.set(w.key,{option:w,enable,fields});
      }
      if(!choices.size)windows.append(node('p','No other normalized quota windows have been reported.','help'));card.append(windows);accountRoot.append(card);accountRows.set(a.ID,{a,own,mode,priority,inputs,windows:windowRows});
    }
    const draft=()=>({providers:Object.fromEntries([...providerRows].map(([id,r])=>[id,{touched:r.touched(),values:Object.fromEntries([...r.fields].map(([k,f])=>[k,f.raw()]))}])),accounts:Object.fromEntries([...accountRows].map(([id,r])=>[id,{own:r.own.value,mode:r.mode.value,priority:r.priority.raw(),values:Object.fromEntries([...r.inputs].map(([k,f])=>[k,f.raw()])),windows:Object.fromEntries([...r.windows].map(([key,w])=>[key,{enabled:w.enable.checked,values:Object.fromEntries([...w.fields].map(([k,f])=>[k,f.raw()]))}]))}]))});
    return {draft,read:()=>{
      const out={account_priority:{},account_ownership:{},account_reserves:{},provider_reserves:{},window_reserves:{}};
      for(const [id,r]of providerRows){const settings=Object.fromEntries([...r.fields].map(([path,f])=>[path,f.read()]));if(id==='claude')out.shared_claude=settings;else if(r.saved||settings.enabled||r.touched())out.provider_reserves[id]=settings;}
      for(const [id,r]of accountRows){const priority=r.priority.read();if(priority!==0)out.account_priority[id]=priority;if(r.own.value!=='inherit')out.account_ownership[id]=r.own.value==='owned';if(r.mode.value==='custom')out.account_reserves[id]=Object.fromEntries([...r.inputs].map(([path,f])=>[path,f.read()]));const rules={};for(const [key,w]of r.windows)if(w.enable.checked)rules[key]={group:w.option.group,window:w.option.window,...Object.fromEntries([...w.fields].map(([path,f])=>[path,f.read()]))};if(Object.keys(rules).length)out.window_reserves[id]=rules;}
      return out;
    }};
  }
  return {clientEditor,subscriptionEditor};
})();
