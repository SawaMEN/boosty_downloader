'use strict';
// The onboarding wizard is an explicit opt-in: connecting Boosty never starts downloads.
let subscriptionItems=[];
let importRefreshTimer=0;

async function openSubscriptionWizard(){
  if (!state.boosty){toast('Сначала подключите аккаунт Boosty',true);nav('boosty');return;}
  $('subscriptionDialog').hidden=false;
  $('subscriptionError').textContent='';
  $('subscriptionStatus').textContent='Получаем список авторов из Boosty…';
  $('subscriptionList').replaceChildren();
  $('subscriptionSubmit').disabled=true;
  try{
    const data=await api('/api/subscriptions');
    subscriptionItems=(Array.isArray(data.authors)?data.authors:[]).map(item=>({...item,selected:false}));
    $('subscriptionStatus').textContent=subscriptionItems.length ? `Найдено авторов: ${subscriptionItems.length}${data.partial?' · список может быть неполным':''}` : 'Активные подписки и отслеживаемые блоги не найдены.';
    renderSubscriptions();
  }catch(e){subscriptionItems=[];$('subscriptionStatus').textContent='Не удалось прочитать подписки';$('subscriptionError').textContent=e.message;$('subscriptionList').replaceChildren();updateSubscriptionSelection();}
}
function renderSubscriptions(){
  const node=$('subscriptionList');node.replaceChildren();
  const query=$('subscriptionSearch').value.trim().toLowerCase();
  for(const author of subscriptionItems){
    if(query&&!`${author.title} ${author.blog}`.toLowerCase().includes(query))continue;
    const label=el('label','subscription-row');
    const checkbox=el('input','subscription-choice');checkbox.type='checkbox';checkbox.value=author.blog;checkbox.checked=Boolean(author.selected);checkbox.addEventListener('change',()=>{author.selected=checkbox.checked;updateSubscriptionSelection()});
    const letter=text('span',author.title.slice(0,1).toUpperCase()||'B','subscription-avatar');
    const body=el('span','subscription-info');body.append(text('strong',author.title));body.append(text('small',`boosty.to/${author.blog} · ${author.level|| (author.paid?'Платная подписка':'Подписка')}${author.paused?' · приостановлена':''}`));
    label.append(checkbox,letter,body);node.append(label);
  }
  if(!node.children.length)node.append(empty('Нет подходящих авторов','Измените строку поиска или подключите другой аккаунт Boosty.'));
  updateSubscriptionSelection();
}
function updateSubscriptionSelection(){
  const n=subscriptionItems.filter(a=>a.selected).length;
  $('subscriptionSelected').textContent=`Выбрано: ${n} из ${subscriptionItems.length} · максимум 20 за один запуск`;
  $('subscriptionSubmit').disabled=n===0||n>20;
}
async function submitSubscriptionImport(){
  const authors=subscriptionItems.filter(a=>a.selected).map(a=>a.blog);
  if(!authors.length||authors.length>20)return;
  const button=$('subscriptionSubmit');button.disabled=true;$('subscriptionError').textContent='';
  try{
    const body={authors,maxPosts:Number($('subscriptionMaxPosts').value),includeComments:$('optionComments').checked,includeFiles:$('optionFiles').checked,includeVideos:$('optionVideo').checked,includeAudio:$('optionAudio').checked,autoSync:$('optionSync').checked};
    const r=await api('/api/subscriptions/import',{method:'POST',body});
    $('subscriptionDialog').hidden=true;
    nav('authors');await refreshImports();
    const count=r.imports?.length||0;
    toast(count?`Начат поиск публикаций у ${count} авторов. Архивы появятся в медиатеке.`:'Выбранные авторы уже обрабатываются.');
  }catch(e){$('subscriptionError').textContent=e.message;toast(e.message,true)}
  finally{updateSubscriptionSelection();}
}
async function refreshImports(){
  if(!state.user)return;
  const target=$('authorImports');if(!target)return;
  try{
    const items=await api('/api/imports');
    target.replaceChildren();
    if(!items.length){target.append(empty('Импортов пока нет','Подключите Boosty и выберите авторов для создания копий.'));return;}
    for(const item of items.slice(0,50)){
      const card=el('article','import-item');const info=el('div','import-main');
      info.append(text('strong',item.title||item.author));
      let status={queued:'Ожидает',running:'Поиск постов…',done:'Поиск завершён',failed:'Ошибка поиска',canceled:'Поиск остановлен'}[item.state]||item.state;
      if(item.limited)status+=' · достигнут выбранный лимит';
      info.append(text('div',`${status} · просмотрено ${item.scanned}, добавлено ${item.queued}, пропущено ${item.skipped}`,'muted small'));
      info.append(text('div',`Изменено: ${new Date(item.updatedAt).toLocaleString('ru-RU')}`,'muted small'));
      if(item.autoSync)info.append(text('div','Автопроверка новых публикаций включена','muted small'));
      if(item.error)info.append(text('div',item.error,'error'));
      card.append(info);
      const b=text('button','Открыть автора','tiny-btn');b.onclick=()=>chooseAuthor(item.author);card.append(b);
      if(item.state==='done'){
        const toggle=text('button',item.autoSync?'Выключить автопроверку':'Включить автопроверку','tiny-btn secondary');
        toggle.onclick=async()=>{
          toggle.disabled=true;
          try{await api('/api/imports/'+encodeURIComponent(item.id)+'/schedule',{method:'PUT',body:{autoSync:!item.autoSync}});await refreshImports();toast(item.autoSync?'Автопроверка отключена':'Автопроверка включена');}
          catch(e){toast(e.message,true);toggle.disabled=false;}
        };
        card.append(toggle);
      }
      if(item.state==='running'||item.state==='queued'){const cancel=text('button','Остановить поиск','tiny-btn delete');cancel.onclick=async()=>{if(!confirm('Остановить поиск новых постов автора? Уже поставленные в очередь загрузки останутся.'))return;try{await api('/api/imports/'+encodeURIComponent(item.id)+'/cancel',{method:'POST'});refreshImports()}catch(e){toast(e.message,true)}};card.append(cancel)}
      target.append(card);
    }
    if(items.some(i=>i.state==='queued'||i.state==='running')){
      clearTimeout(importRefreshTimer);
      importRefreshTimer=setTimeout(refreshImports,3500);
    }
  }catch(e){target.replaceChildren(text('p',e.message,'error'))}
}
function subscriptionEvents(){
  $('pickSubscriptions').addEventListener('click',openSubscriptionWizard);
  $('pickSubscriptionsFromBoosty').addEventListener('click',openSubscriptionWizard);
  $('closeSubscriptions').addEventListener('click',()=>{$('subscriptionDialog').hidden=true;});
  $('subscriptionDialog').addEventListener('click',e=>{if(e.target===$('subscriptionDialog'))$('subscriptionDialog').hidden=true;});
  $('subscriptionSearch').addEventListener('input',()=>{
    // Preserve checked items even when temporarily filtered out.
    const query=$('subscriptionSearch').value.trim().toLowerCase(),list=$('subscriptionList');list.replaceChildren();
    for(const a of subscriptionItems){if(query&&!`${a.title} ${a.blog}`.toLowerCase().includes(query))continue;
      const label=el('label','subscription-row'),box=el('input','subscription-choice');box.type='checkbox';box.value=a.blog;box.checked=Boolean(a.selected);box.onchange=()=>{a.selected=box.checked;updateSubscriptionSelection()};
      const avatar=text('span',a.title.slice(0,1).toUpperCase(),'subscription-avatar');const info=el('span','subscription-info');info.append(text('strong',a.title));info.append(text('small',a.blog));label.append(box,avatar,info);list.append(label);
    }
    updateSubscriptionSelection();
  });
  $('subscriptionSelectAll').onclick=()=>{for(const a of subscriptionItems)a.selected=true;syncSubscriptionChecks();};
  $('subscriptionSelectNone').onclick=()=>{for(const a of subscriptionItems)a.selected=false;syncSubscriptionChecks();};
  $('subscriptionSubmit').onclick=submitSubscriptionImport;
  $('refreshImports').onclick=refreshImports;
}
function syncSubscriptionChecks(){for(const box of document.querySelectorAll('.subscription-choice'))box.checked=Boolean(subscriptionItems.find(a=>a.blog===box.value)?.selected);updateSubscriptionSelection();}
