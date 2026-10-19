package aiapp

import("encoding/json";"os";"path/filepath";"strings";"testing";"go-lua-crawler/internal/agentdata")
func TestDeliverySavedTraceContextKeepsRecentNecessaryReadText(t *testing.T){
 root:=os.Getenv("GORELAY_DELIVERY_TRACE_DIR");if root==""{t.Skip("set GORELAY_DELIVERY_TRACE_DIR for the frozen real-trace gate")}
 raw,err:=os.ReadFile(filepath.Join(root,"first-network-restored","observed.json"));if err!=nil{t.Fatal(err)}
 var saved struct{Run agentdata.Run `json:"run"`;Operations []agentdata.Operation `json:"operations"`}
 if err=json.Unmarshal(raw,&saved);err!=nil{t.Fatal(err)}
 for _,rounds:=range []int{5,7}{
  s:=LoopState{Run:saved.Run};_ =json.Unmarshal([]byte(s.Run.SpecJSON),&s.Spec);_ =json.Unmarshal([]byte(s.Run.BudgetJSON),&s.Budget)
  s.Run.Status="reviewing";s.Run.Cancelled=false
  models:=0;expected:=map[string]string{}
  for _,op:=range saved.Operations{
   if op.Kind=="model"{models++;if models>rounds{break}}
   s.Operations=append(s.Operations,op)
   if op.Kind=="model"&&op.Stage=="succeeded"{var response ChatResponse;_ =json.Unmarshal([]byte(op.ResultJSON),&response);s.Messages=append(s.Messages,response.Message)}
   if strings.HasPrefix(op.Key,"tool:")&&(op.Stage=="succeeded"||op.Stage=="failed"){
    var reply ToolResult;_ =json.Unmarshal([]byte(op.ResultJSON),&reply)
    s.Messages=append(s.Messages,Message{Role:"tool",ToolCallID:reply.CallID,Content:op.ResultJSON})
    var data struct{Text string `json:"text"`};_ =json.Unmarshal(reply.Data,&data)
    if op.Kind=="read_data"&&len([]rune(data.Text))>200{runes:=[]rune(data.Text);expected[reply.CallID]=string(runes[len(runes)-120:])}
   }
  }
  got,e:=CompactContext(s,s.Messages);if e!=nil{t.Fatal(e)}
  t.Logf("rounds=%d request_bytes=%d read_slices=%d",rounds,contextRequestBytes(s,got),len(expected))
  for id,tail:=range expected{
   visible:=false
   for _,m:=range got{if m.ToolCallID==id{var reply ToolResult;_ =json.Unmarshal([]byte(m.Content),&reply);var data map[string]any;_ =json.Unmarshal(reply.Data,&data);text,_:=data["text"].(string);visible=strings.Contains(text,tail)}}
   if !visible{t.Errorf("rounds=%d read %s lost its useful tail before any task assessment/summary",rounds,id)}
  }
 }
}
