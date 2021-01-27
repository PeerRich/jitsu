package events

import "github.com/jitsucom/eventnative/drivers"

const (
	EventnKey       = "eventn_ctx"
	collectionIdKey = "collection_id"
	TimeChunkKey    = "time_interval"

	EventIdKey = "event_id"

	EventnCtxEventId = "eventn_ctx_event_id"
)

//EnrichWithEventId put eventId to EventnKey_EventIdKey key if it doesn't exist there or if there is an empty string
func EnrichWithEventId(object map[string]interface{}, eventId string) {
	eventnObject, ok := object[EventnKey]
	if !ok {
		eventnObject = map[string]interface{}{EventIdKey: eventId}
		object[EventnKey] = eventnObject
	} else {
		if eventn, ok := eventnObject.(map[string]interface{}); ok {
			val, ok := eventn[EventIdKey]
			if !ok || val == "" {
				eventn[EventIdKey] = eventId
			}
		} else {
			object[EventnCtxEventId] = eventId
		}
	}
}

func EnrichWithCollection(object map[string]interface{}, collection string) {
	eventnObject, ok := object[EventnKey]
	if !ok {
		eventnObject = map[string]interface{}{collectionIdKey: collection}
		object[EventnKey] = eventnObject
	} else {
		if eventn, ok := eventnObject.(map[string]interface{}); ok {
			if _, ok := eventn[collectionIdKey]; !ok {
				eventn[collectionIdKey] = collection
			}
		} else {
			object[EventnKey+"_"+collectionIdKey] = collection
		}
	}
}

func EnrichWithTimeInterval(object map[string]interface{}, interval string) {
	object[EventnKey+"_"+TimeChunkKey] = interval
func EnrichWithTimeInterval(object map[string]interface{}, interval *drivers.TimeInterval) {
	object[EventnKey+"_"+TimeChunkKey] = interval.String()
	object[EventnKey+"_interval_start"] = timestamp.ToISOFormat(interval.LowerEndpoint())
	object[EventnKey+"_interval_end"] = timestamp.ToISOFormat(interval.UpperEndpoint())
}
