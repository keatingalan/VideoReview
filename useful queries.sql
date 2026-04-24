select *,
	json_extract(message,'$.apparatus') apparatus,
	json_extract(message,'$.club') club,
	json_extract(message,'$.competitor') competitor,
	json_extract(message,'$.status') status,
	datetime(cast(json_extract(message,'$.time') as real)/1000,'unixepoch','localtime') time_human
	
 from messages 
 where 
 --competitor="824" and
 apparatus="Beam" order by time_human desc





 select 
	datetime(time_start/1000, 'unixepoch','localtime') as start, 
	datetime(time_stop/1000, 'unixepoch','localtime') as stop, 
	*
from routines 
where (time_stop-time_start)/1000<30



select 
	id,
	json_extract(message,'$.fullMessage') orig_msg,
	json_extract(message,'$.apparatus') apparatus,
	json_extract(message,'$.club') club,
	json_extract(message,'$.competitor') competitor,
	json_extract(message,'$.status') status,
	datetime(cast(json_extract(message,'$.time') as real)/1000,'unixepoch','localtime') time_human,
	json_extract(message,'$.time') ts
	, json_extract(message,'$.fullMessage.NewScore.Session._text') session_new,json_extract(message,'$.fullMessage.NowUp.Session._text') session_now
	
 from messages 
 where 
 true--competitor="891" --and
 --apparatus="Beam" 
 order by ts desc