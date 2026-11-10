'use strict';
const $ = id => document.getElementById(id);
const state = {user:null, post:null, jobs:[], boosty:false, feedAuthor:'', nextOffset:'', canMore:false, register:false, authMode:'login', toastTimer:0};
const titles = {home:'Обзор',browse:'Публикации',authors:'Авторы',library:'Медиатека',shared:'Общие архивы',boosty:'Аккаунт Boosty',settings:'Настройки'};
const optionNames=['Comments','Files','Video','Audio','Sync'];
const text = (tag,value,cls) => {const node=document.createElement(tag);node.textContent=String(value??'');if(cls)node.className=cls;return node};
function el(tag,cls,attrs={}){const node=document.createElement(tag);if(cls)node.className=cls;for(const [key,value] of Object.entries(attrs))node.setAttribute(key,value);return node}
function safeLink(textValue,href,download=false){const a=text('a',textValue,'tiny-btn');a.href=href;if(download)a.download='';else if(href.startsWith('http')){a.target='_blank';a.rel='noopener noreferrer'}return a}
function empty(title,detail=''){const root=el('div','empty-state');root.append(text('div','◇'));root.append(text('strong',title));if(detail)root.append(text('p',detail));return root}
function toast(msg,isError=false){clearTimeout(state.toastTimer);const t=$('toast');t.textContent=msg;t.classList.toggle('fail',isError);t.hidden=false;state.toastTimer=setTimeout(()=>t.hidden=true,5500)}
async function api(path,opts={}){
  const options={credentials:'same-origin',cache:'no-store',...opts};
  options.headers={...opts.headers};
  if(opts.body && typeof opts.body==='object' && !(opts.body instanceof FormData)){
    options.body=JSON.stringify(opts.body);options.headers['Content-Type']='application/json';
  }
  if(options.method && !['GET','HEAD'].includes(options.method))options.headers['X-BD-Request']='1';
  const resp=await fetch(path,options);
  let result={};try{result=await resp.json()}catch{};
  if(!resp.ok)throw new Error(result.error||`HTTP ${resp.status}`);
  return result;
}
function switchAuth(mode){state.authMode=mode;$('authTitle').textContent=mode==='register'?'Создать аккаунт':'Вход в загрузчик';$('submitLogin').firstChild.textContent=mode==='register'?'Зарегистрироваться ':'Войти в медиатеку ';$('loginPass').autocomplete=mode==='register'?'new-password':'current-password';$('switchAuth').textContent=mode==='register'?'У меня есть аккаунт':'Создать аккаунт';$('authError').textContent=''}
async function publicConfig(){try{const data=await api('/api/public-config');state.register=data.registrationOpen;$('switchAuth').hidden=!state.register;$('registrationMessage').textContent=state.register?'Нет аккаунта?':'Регистрация закрыта администратором';if(!state.register && state.authMode==='register')switchAuth('login')}catch{$('registrationMessage').textContent='Регистрация временно недоступна'}}
async function init(){await publicConfig();try{state.user=await api('/api/account');enterApp()}catch{$('appScreen').hidden=true;$('loginScreen').hidden=false}}
async function onLogin(event){event.preventDefault();$('authError').textContent='';$('submitLogin').disabled=true;
 try{const method=state.authMode==='register'?'register':'login';await api(`/api/account/${method}`,{method:'POST',body:{username:$('loginUser').value.trim(),password:$('loginPass').value}});state.user=await api('/api/account');$('loginPass').value='';enterApp()}
 catch(e){$('authError').textContent=e.message}finally{$('submitLogin').disabled=false}}
function enterApp(){$('loginScreen').hidden=true;$('appScreen').hidden=false;$('userName').textContent=state.user.username;$('userInitial').textContent=state.user.username.slice(0,1).toUpperCase();$('userRole').textContent=state.user.admin?'Администратор':'Участник';updatePermissionUI();nav('home');refreshAll();if(state.user.admin)loadAdminPanel()}
function nav(page){if(!titles[page])return;for(const node of document.querySelectorAll('.page'))node.classList.toggle('active',node.id===`page-${page}`);for(const node of document.querySelectorAll('[data-page]'))node.classList.toggle('active',node.dataset.page===page);$('pageCrumb').textContent=titles[page];$('sidebar').classList.remove('open');if(page==='library'){refreshJobs();loadAuthors()}if(page==='authors'){loadAuthors();refreshImports();}if(page==='shared'&&permissionValue('browseShared'))refreshShared();if(page==='boosty')refreshBoosty();if(page==='settings'&&state.user?.admin)loadAdminPanel()}
function jobState(j){const states={done:'Готово',partial:'Частично',queued:'В очереди',running:'Загружается',failed:'Ошибка'};return states[j.state]||j.state}
function buildJob(j, shared=false){const item=el('article','job-item');item.append(text('div','▧','job-icon'));const body=el('div','job-main');body.append(text('div',j.title||j.url,'job-title'));body.append(text('div',`${j.author}${shared?' · от '+j.ownerName:''} · ${j.doneFiles}/${j.totalFiles} файлов`,'job-sub'));
 if(j.archiveUpdatedAt){const when=new Date(j.archiveUpdatedAt);body.append(text('div',`Архив обновлён: ${when.toLocaleString('ru-RU')}`,'job-sub archive-date'));}
 else body.append(text('div',`Добавлено: ${new Date(j.createdAt).toLocaleString('ru-RU')}`,'job-sub archive-date'));
 if(j.sharedForAll)body.append(text('div','◉ Доступен всем участникам','job-sub public-archive'));
 if(j.error)body.append(text('div',j.error,'job-sub'));
 if(j.state==='running' || j.state==='queued'){const track=el('div','progress'),bar=el('span');bar.style.width=`${Math.min(100,Math.floor(j.doneFiles/Math.max(1,j.totalFiles)*100))}%`;track.append(bar);body.append(track)}
 const actions=el('div','job-buttons');
 if(['done','partial'].includes(j.state)){
   actions.append(safeLink('Открыть копию ↗',`/library/${encodeURIComponent(j.id)}/index.html`));
   actions.append(safeLink('Скачать ZIP',`/api/jobs/${encodeURIComponent(j.id)}/zip`,true));
 }
 if(!shared && permissionValue('createArchives') && ['done','partial','failed'].includes(j.state)){
  const b=text('button','⟳ Обновить','tiny-btn');b.onclick=async()=>{try{await api(`/api/jobs/${j.id}/retry`,{method:'POST'});toast('Задача добавлена в очередь');refreshJobs()}catch(e){toast(e.message,true)}};actions.append(b)
 }
 if(!shared && ['done','partial'].includes(j.state)) {
  actions.append(libraryAction('Проверить файлы',()=>verifyArchiveUI(j)));
  if(permissionValue('shareDirect')||permissionValue('sharePublic')) actions.append(libraryAction('Поделиться',()=>openShareDialog(j)));
 }
 if(!shared && permissionValue('deleteArchives') && !['queued','running'].includes(j.state)) actions.append(libraryAction('Удалить',()=>deleteArchivedPost(j),'delete'));
 if(actions.children.length)body.append(actions);
 item.append(body);const badge=text('span',jobState(j),`status-badge ${j.state}`);item.append(badge);return item;
}
function renderLibraryJobs() {
 const container=$('libraryJobs'); if(!container)return;
 container.replaceChildren();
 const data=state.jobs.filter(j=>!archiveFilter||j.author===archiveFilter);
 if(!data.length)container.append(empty('Архивы не найдены','Откройте публикацию и сохраните её полную копию.'));
 else data.forEach(j=>container.append(buildJob(j)));
 $('deleteAuthorArchives').disabled=!archiveFilter;
}
async function refreshJobs(){
 if(!state.user)return;
 try{
  state.jobs=await api('/api/jobs');
  const recent=$('recentJobs');recent.replaceChildren();
  if(!state.jobs.length)recent.append(empty('Здесь пока пусто','Откройте публикацию и сохраните полную копию.'));
  else state.jobs.slice(0,4).forEach(j=>recent.append(buildJob(j)));
  renderLibraryJobs();
  $('statArchives').textContent=state.jobs.filter(j=>['done','partial'].includes(j.state)).length;
  $('statQueued').textContent=state.jobs.filter(j=>['queued','running'].includes(j.state)).length;
  if($('archiveAuthorFilter'))loadAuthors();
 }catch(e){toast(e.message,true)}
}
async function refreshBoosty(){if(!state.user)return;try{const b=await api('/api/auth');state.boosty=b.connected;$('boostyDot').classList.toggle('connected',b.connected);$('topBoosty').textContent=b.connected?'Boosty подключён':'Boosty не подключён';$('connectionText').textContent=b.connected?'Аккаунт Boosty подключён':'Подключите аккаунт Boosty';$('connectionDetail').textContent=b.connected?'Можно читать и скачивать доступные вашей подписке посты.':'Открытые посты доступны и без подключения. Для закрытых нужен ваш Boosty.';$('disconnectBoosty').hidden=!b.connected;$('statAccount').textContent=b.connected?'Активен':'Гость'}catch(e){toast(e.message,true)}}
async function refreshAll(){await Promise.all([refreshJobs(),refreshBoosty(),...(permissionValue('browseShared')?[refreshShared()]:[])])}
function urlParams(url){return encodeURIComponent(url)}
function streamUrl(url,index,inline=false,quality=''){return `/api/stream?url=${urlParams(url)}&index=${index}${inline?'&inline=1':''}${quality?'&quality='+encodeURIComponent(quality):''}`}
function renderMedia(p,m){const wrap=el('div','post-media');const title=text('strong',m.title||({'ok_video':'Видео','audio_file':'Аудио','image':'Изображение','file':'Файл'}[m.type]||'Медиа'));wrap.append(title);
 const actions=el('div','media-tools');let mediaNode;
 if(m.type==='ok_video'){
   mediaNode=el('video');mediaNode.controls=true;mediaNode.preload='metadata';mediaNode.playsInline=true;mediaNode.src=streamUrl(p.source,m.index,true);
   const qualityOptions=Array.isArray(m.qualities)?m.qualities:[];
   if(qualityOptions.length>1){const selector=el('select');selector.setAttribute('aria-label','Качество видео');for(const quality of qualityOptions){const option=el('option');option.value=quality;option.textContent=quality.replaceAll('_',' ').toUpperCase();selector.append(option)}selector.onchange=()=>{const wasPlaying=!mediaNode.paused,time=mediaNode.currentTime;mediaNode.src=streamUrl(p.source,m.index,true,selector.value);mediaNode.addEventListener('loadedmetadata',()=>{if(Number.isFinite(time)&&time>0)mediaNode.currentTime=time;if(wasPlaying)mediaNode.play().catch(()=>{})},{once:true})};actions.append(selector)}
   const storageKey=`boosty-watch:${state.user.id}:${p.author}:${p.postId}:${m.index}`;
   mediaNode.addEventListener('loadedmetadata',()=>{const saved=Number(localStorage.getItem(storageKey));if(saved>0&&saved<mediaNode.duration-7)mediaNode.currentTime=saved});
   mediaNode.addEventListener('timeupdate',()=>{if(Number.isFinite(mediaNode.currentTime))localStorage.setItem(storageKey,String(Math.floor(mediaNode.currentTime)))});
   mediaNode.addEventListener('ended',()=>localStorage.removeItem(storageKey));
   mediaNode.addEventListener('error',()=>{const warning=wrap.querySelector('.play-error');if(!warning){const msg=text('p','Поток не поддерживается браузером или CDN отклонил запрос. Попробуйте скачать файл либо сохранить пост. HLS/DASH пока поддерживаются не всеми браузерами.','muted small play-error');wrap.append(msg)}});
 } else if(m.type==='audio_file') {mediaNode=el('audio');mediaNode.controls=true;mediaNode.preload='none';mediaNode.src=streamUrl(p.source,m.index,true)}
 else if(m.type==='image'){mediaNode=el('img');mediaNode.loading='lazy';mediaNode.src=streamUrl(p.source,m.index,true);mediaNode.alt=m.title||'Изображение'}
 if(mediaNode && permissionValue('streamMedia'))wrap.append(mediaNode);
 if(permissionValue('streamMedia'))actions.append(safeLink('↓ Скачать на компьютер',streamUrl(p.source,m.index),true));wrap.append(actions);return wrap;
}
async function openPost(url){if(!url)throw new Error('Укажите ссылку на публикацию');nav('browse');const viewer=$('postViewer');viewer.hidden=false;$('postTitle').textContent='Загрузка публикации…';$('postBlocks').replaceChildren();$('commentsArea').replaceChildren();$('loadComments').hidden=false;
 const p=await api('/api/post?url='+urlParams(url));state.post=p;$('postTitle').textContent=p.title||'Без названия';$('postAuthor').textContent=p.author;$('postMeta').textContent=`${p.hasAccess?'Доступно для просмотра':'Доступ ограничен подпиской'} · ${p.media.length} медиа · ${p.comments} комментариев`;$('openOriginal').onclick=()=>window.open(p.source,'_blank','noopener,noreferrer');
 if(!p.hasAccess){$('postBlocks').append(empty('Требуется доступ Boosty','Подключите аккаунт с подпиской на автора, чтобы прочитать закрытый пост.'));return}
 for(const block of p.blocks||[]){if(['text','header','list','link'].includes(block.type)){if(block.text){const tag=block.type==='header'?'h2':block.type==='list'?'li':'p';const node=text(tag,block.text,'post-paragraph');if(tag==='li'){const list=el('ul');list.append(node);$('postBlocks').append(list)}else $('postBlocks').append(node)}}else{const m=p.media.find(x=>x.index===block.index);if(m)$('postBlocks').append(renderMedia(p,m))}}
 if(!(p.blocks||[]).length)$('postBlocks').append(empty('Нет содержимого','Публикация может быть ограничена Boosty'));
 viewer.scrollIntoView({behavior:'smooth',block:'start'});
}
function showComment(c){const d=el('details','comment-item');d.open=true;const s=el('summary');s.textContent=c.author||'Пользователь';d.append(s);if(c.createdAt){const dt=Number(c.createdAt)>1e12?Number(c.createdAt):Number(c.createdAt)*1000;d.append(text('small',new Date(dt).toLocaleString('ru-RU')))}if(c.text)d.append(text('p',c.text));if(c.replies?.length){const wrap=el('div','comment-nested');c.replies.forEach(ch=>wrap.append(showComment(ch)));d.append(wrap)}return d}
async function loadComments(){const p=state.post;if(!p)return;const area=$('commentsArea');area.replaceChildren(text('p','Загружаем комментарии…','muted'));
 try{const r=await api('/api/comments?url='+urlParams(p.source));area.replaceChildren();area.append(text('h3',`Комментарии — ${r.received} ${r.partial?'(частично)':''}`));if(r.partial)area.append(text('p',r.reason||'Boosty ограничил количество доступных ответов','muted'));if(!r.comments?.length)area.append(empty('Комментариев пока нет'));else r.comments.forEach(c=>area.append(showComment(c)))}catch(e){area.replaceChildren(text('p',e.message,'error'))}}
async function loadFeed(reset=false){const author=$('feedAuthor').value.trim();if(!author)throw new Error('Введите автора Boosty');const target=$('feedResults');if(reset){target.replaceChildren();state.feedAuthor=author;state.nextOffset='';$('pinFeedAuthor').hidden=false;loadAuthors()}
 const r=await api('/api/feed?author='+encodeURIComponent(author)+(state.nextOffset?'&offset='+encodeURIComponent(state.nextOffset):''));state.nextOffset=r.nextOffset||'';state.canMore=r.hasMore;$('feedMore').hidden=!r.hasMore;
 if(!r.posts?.length&&!target.children.length){target.append(empty('Публикации не найдены','Убедитесь, что имя автора указано правильно.'));return}
 for(const p of r.posts||[]){const item=el('button','feed-card');item.append(text('span',p.hasAccess?'● Доступен':'○ Закрытый','eyebrow'));item.append(text('h3',p.title||'Публикация'));item.append(text('div',`${new Date(p.publishTime*1000).toLocaleDateString('ru-RU')} · ${p.mediaCount} медиа · ${p.comments} комментариев`,'meta'));item.onclick=()=>openPost(p.url).catch(e=>toast(e.message,true));target.append(item)}
}
function showArchiveDialog() {if(!permissionValue('createArchives')){toast('Архивация отключена администратором',true);return}if(!state.post||!state.post.hasAccess){toast('Сначала откройте доступную публикацию',true);return}for(const key of optionNames){const source=$('option'+key),target=$('archive'+key);target.checked=source.checked}$('archiveDialogTitle').textContent='Сохранить «'+(state.post.title||'публикацию')+'»';$('archiveDialog').hidden=false}
async function submitArchive(){const url=state.post?.source;if(!url)return;try{const body={url,includeComments:$('archiveComments').checked,includeFiles:$('archiveFiles').checked,includeVideos:$('archiveVideo').checked,includeAudio:$('archiveAudio').checked,autoSync:$('archiveSync').checked};await api('/api/jobs',{method:'POST',body});$('archiveDialog').hidden=true;toast('Публикация добавлена в очередь');nav('library');refreshJobs()}catch(e){toast(e.message,true)}}
async function connectBoosty(){if(!permissionValue('connectBoosty'))return toast('Подключение отключено администратором',true);try{let body;const tab=document.querySelector('.tab.active')?.dataset.connect;if(tab==='cookie')body={method:'cookie',value:$('cookieValue').value.trim()};else body={method:'tokens',accessToken:$('accessToken').value.trim(),refreshToken:$('refreshToken').value.trim(),deviceId:$('deviceId').value.trim()};await api('/api/auth',{method:'POST',body});for(const id of ['cookieValue','accessToken','refreshToken','deviceId'])$(id).value='';$('boostyFeedback').textContent='Аккаунт подключён';toast('Токены Boosty сохранены');await refreshBoosty();if(permissionValue('importAuthors'))await openSubscriptionWizard()}catch(e){$('boostyFeedback').textContent=e.message;toast(e.message,true)}}

function events(){
 $('loginForm').addEventListener('submit',onLogin);
 $('switchAuth').onclick=()=>switchAuth(state.authMode==='login'?'register':'login');
 $('logout').onclick=async()=>{await api('/api/account/logout',{method:'POST'});state.user=null;state.post=null;state.jobs=[];subscriptionItems=[];$('subscriptionDialog').hidden=true;$('appScreen').hidden=true;$('loginScreen').hidden=false;switchAuth('login');publicConfig()};
 $('mobileMenu').onclick=()=>$('sidebar').classList.toggle('open');
 for(const b of document.querySelectorAll('[data-page]'))b.onclick=()=>nav(b.dataset.page);
 for(const b of document.querySelectorAll('[data-go]'))b.onclick=()=>nav(b.dataset.go);
 $('openForm').addEventListener('submit',async e=>{e.preventDefault();try{await openPost($('postUrl').value.trim())}catch(err){toast(err.message,true)}});
 $('feedForm').addEventListener('submit',e=>{e.preventDefault();loadFeed(true).catch(err=>toast(err.message,true))});
 $('feedMore').onclick=()=>loadFeed(false).catch(e=>toast(e.message,true));
 $('loadComments').onclick=loadComments;
 $('startArchive').onclick=showArchiveDialog;
 $('closeArchive').onclick=()=>$('archiveDialog').hidden=true;
 $('archiveDialog').addEventListener('click',e=>{if(e.target===$('archiveDialog'))$('archiveDialog').hidden=true});
 $('confirmArchive').onclick=submitArchive;
 $('refreshJobs').onclick=refreshJobs;
 for(const b of document.querySelectorAll('[data-connect]'))b.onclick=()=>{for(const tab of document.querySelectorAll('[data-connect]'))tab.classList.toggle('active',tab===b);$('cookieFields').hidden=b.dataset.connect!=='cookie';$('tokenFields').hidden=b.dataset.connect!=='tokens'};
 $('connectCookie').onclick=connectBoosty;$('connectTokens').onclick=connectBoosty;
 $('disconnectBoosty').onclick=async()=>{if(!confirm('Отключить аккаунт Boosty? Сохранённые архивы останутся.'))return;try{await api('/api/auth',{method:'DELETE'});refreshBoosty();toast('Boosty отключён')}catch(e){toast(e.message,true)}};
 
 for(const key of optionNames){const control=$('option'+key);control.checked=localStorage.getItem('option'+key)==='true'||(localStorage.getItem('option'+key)===null&&key!=='Sync');control.onchange=()=>localStorage.setItem('option'+key,String(control.checked))}
 libraryEvents();
 subscriptionEvents();
 adminEvents();
 setInterval(()=>{if(state.user&&document.visibilityState==='visible')refreshJobs().catch(()=>{})},15000);
}
document.addEventListener('DOMContentLoaded',()=>{events();init()});
