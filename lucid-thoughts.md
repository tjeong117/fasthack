I thinking the other day. In the context of Autoresearch driven by parallel multi-agent that are recursive.  
Lets say I have 5 instances of Agents with its own sandbox tackling the same autoresearch problem. Optimizing a metric called X_optimize. Assume that all of these agents have their own sandbox, GPU for running ML experiments (ML autoreserach), as well as they are all attatched to the same Recursive language model harness with max depth > 1. 


So lets formalize this. a Successful autoresaerch trajectory might look like this for agent-1. (out of agent-2, .. agent-n )
A -> B -> .. -> X -> Y -> Z. 
and f(s) = X_s wher X is the metric and s is that sate from {A .. Z or more} and we want to miniimiz X_s 

note: there will be cases wehre f(A) <= f(B) althought that isn't ideal. 

My agrument is that there could be common states and transiiton functions to the next state. That can reduce significant thinking time and also let other agents know where it is and what actions can lead to what. Like for exmaple if my agent-1 is on State C. and the other agent got to state C. while agent-1 has explored a lot of differnet actions then the other agent can build off of their search space. 
Additionally I was thinking that this kinda thinking can be userd for subage3nt dispatch


Subagents are only a recent phenomena as agentic long horizon is possible and agents are good at it .
but issues: slow, orchestrator agent needs to wati until subagent ffinishes. this will be a bigger issue if there is a lot more child and grandchild. 

so maybne the way of doing this is just start from many agents type of way or 1 entry way, but have this state tracfker and manager. 

Need to find a clean way of doing this because some of the agentic workflows are resaerching, not just eidting and running code, but also searching, And direct text type of hashing wont work, But some how it should be isomorphic to the original actions that athae agent is doing so that hash a or state vector a is comparable to hash b or state vector b the same way state a and b are. 

This will greatly sspeed up subagent and no reaons to wait on it etc. 


also im thinking a RLM is only good as it explores more ground my thesis too is that starting with a swarm that also maintains context from other agent might be as good as rlm because the benefit of rlm is that the context is sent from parent to child agents 
