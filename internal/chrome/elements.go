package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PageButton 是当前页面上扫出来的一个可点元素。
// 坐标是视口内 CSS 像素，跟截图 viewport 同一坐标系。
type PageButton struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Tag    string `json:"tag"`
	Kind   string `json:"kind"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	InView bool   `json:"inView"`
}

// ButtonScan 一次扫描的结果。
type ButtonScan struct {
	URL   string       `json:"url"`
	Title string       `json:"title"`
	Total int          `json:"total"`
	Items []PageButton `json:"items"`
	At    int64        `json:"at"`
}

var ErrButtonGone = errors.New("页面上的按钮已失效，请刷新")

// scanJS 扫描可点元素。规则：
//   - 只看可见、有实际尺寸、未禁用的元素
//   - 祖先也命中时保留祖先（点击区域更贴近视觉边界）
//   - 排除铺满视口的容器（避免整页 onclick）
//   - 按文档位置从上到下、从左到右排序，编号写回 data-dc 供点击定位
const scanJS = `(function(){
  var SEL='a[href],button,input[type=submit],input[type=button],input[type=reset],summary,[role=button],[role=link],[role=tab],[role=menuitem],[onclick]';
  var all=[].slice.call(document.querySelectorAll(SEL)).slice(0,400);
  var i;
  for(i=0;i<all.length;i++){ all[i].removeAttribute('data-dc'); }
  function label(el){
    var t=(el.innerText||el.textContent||'').replace(/\s+/g,' ').trim();
    if(!t){ t=(el.getAttribute('aria-label')||el.getAttribute('title')||el.getAttribute('placeholder')||el.value||el.getAttribute('alt')||'').trim(); }
    if(!t){ var im=el.querySelector('img[alt]'); if(im&&im.alt) t=im.alt.trim(); }
    return t;
  }
  function box(el){
    var r=el.getBoundingClientRect();
    if(r.width<12||r.height<10) return null;
    var s=getComputedStyle(el);
    if(s.display==='none'||s.visibility==='hidden'||parseFloat(s.opacity)<0.05) return null;
    if(r.width*r.height>innerWidth*innerHeight*0.92) return null;
    return r;
  }
  var keep=[];
  for(i=0;i<all.length;i++){
    var el=all[i];
    if(el.disabled) continue;
    if(el.getAttribute('aria-hidden')==='true') continue;
    var nested=false, p=el.parentElement;
    while(p){ if(all.indexOf(p)>=0){ nested=true; break; } p=p.parentElement; }
    if(nested) continue;
    var r=box(el);
    if(!r) continue;
    var t=label(el);
    if(!t){ t=(el.tagName==='INPUT')?(el.value||el.getAttribute('type')||'输入框'):('('+el.tagName.toLowerCase()+')'); }
    if(t.length>42) t=t.slice(0,41)+'…';
    var role=el.getAttribute('role');
    var kind=role?role:(el.tagName==='A'?'link':(el.tagName==='INPUT'?'input':'button'));
    keep.push({el:el,text:t,tag:el.tagName.toLowerCase(),kind:kind,top:r.top+scrollY,left:r.left,
      inView:(r.bottom>0&&r.top<innerHeight&&r.right>0&&r.left<innerWidth)});
  }
  keep.sort(function(a,b){ return (a.top-b.top)||(a.left-b.left); });
  var n=Math.min(keep.length,60), items=[];
  for(i=0;i<n;i++){
    var o=keep[i];
    o.el.setAttribute('data-dc',i);
    var q=o.el.getBoundingClientRect();
    items.push({id:i,text:o.text,tag:o.tag,kind:String(o.kind).toLowerCase(),
      x:Math.round(q.left+q.width/2),y:Math.round(q.top+q.height/2),inView:o.inView});
  }
  return JSON.stringify({url:location.href,title:document.title,total:keep.length,items:items});
})()`

// pressJS 把目标元素滚到视口中间，返回可点击的中心坐标。
// 用 elementFromPoint 判断中心点是否真被它接住（避免被吸顶导航遮住），
// 接不住就退回 el.click()。
const pressJSTpl = `(function(){
  var el=document.querySelector('[data-dc="%d"]');
  if(!el) return JSON.stringify({ok:false,err:'元素已失效'});
  try{ el.scrollIntoView({block:'center',inline:'center'}); }catch(e){}
  var r=el.getBoundingClientRect();
  if(r.width<2||r.height<2) return JSON.stringify({ok:false,err:'元素不可见'});
  var x=r.left+r.width/2, y=r.top+r.height/2;
  x=Math.max(1,Math.min(x,innerWidth-1)); y=Math.max(1,Math.min(y,innerHeight-1));
  var hit=document.elementFromPoint(x,y);
  var direct=!!(hit&&(hit===el||el.contains(hit)));
  return JSON.stringify({ok:true,x:Math.round(x),y:Math.round(y),direct:direct});
})()`

// ScanButtons 扫描当前页面的可点元素。
func (m *Manager) ScanButtons(ctx context.Context) (*ButtonScan, error) {
	raw, err := m.evalString(ctx, scanJS)
	if err != nil {
		return nil, err
	}
	if raw == "" || raw == "null" {
		return nil, errors.New("页面未就绪")
	}
	var out struct {
		URL   string       `json:"url"`
		Title string       `json:"title"`
		Total int          `json:"total"`
		Items []PageButton `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("解析扫描结果失败: %w", err)
	}
	if out.Items == nil {
		out.Items = []PageButton{}
	}
	return &ButtonScan{URL: out.URL, Title: out.Title, Total: out.Total, Items: out.Items, At: time.Now().UnixMilli()}, nil
}

// PressButton 点击扫描结果里编号为 id 的元素。
// 优先派发真实鼠标事件（能触发 hover/mousedown 等页面逻辑），
// 被遮挡时退回 DOM 的 click()。
func (m *Manager) PressButton(ctx context.Context, id int) error {
	raw, err := m.evalString(ctx, fmt.Sprintf(pressJSTpl, id))
	if err != nil {
		return err
	}
	var pos struct {
		OK     bool   `json:"ok"`
		Err    string `json:"err"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Direct bool   `json:"direct"`
	}
	if err := json.Unmarshal([]byte(raw), &pos); err != nil {
		return fmt.Errorf("定位按钮失败: %w", err)
	}
	if !pos.OK {
		if pos.Err == "" {
			pos.Err = ErrButtonGone.Error()
		}
		return errors.New(pos.Err)
	}

	x, y := float64(pos.X), float64(pos.Y)
	if !pos.Direct {
		_, err := m.evalString(ctx, fmt.Sprintf(
			`(function(){var e=document.querySelector('[data-dc="%d"]');if(!e)return '';e.click();return 'ok';})()`, id))
		return err
	}

	if err := m.Mouse(ctx, "mouseMoved", x, y, 0); err != nil {
		return err
	}
	if err := m.Mouse(ctx, "mousePressed", x, y, 1); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(40 * time.Millisecond):
	}
	return m.Mouse(ctx, "mouseReleased", x, y, 0)
}

func (m *Manager) evalString(ctx context.Context, expr string) (string, error) {
	conn, session, err := m.sess()
	if err != nil {
		return "", err
	}
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := conn.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, &res); err != nil {
		return "", err
	}
	if res.Exception != nil {
		return "", fmt.Errorf("页面脚本异常: %s", res.Exception.Text)
	}
	return res.Result.Value, nil
}
