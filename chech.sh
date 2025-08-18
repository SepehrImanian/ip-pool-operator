NS=ip
POD=nginx-pod

# Pod labels as JSON
POD_LABELS_JSON=$(kubectl get pod "$POD" -n "$NS" -o json | jq -r '.metadata.labels')

# Show policies whose podSelector (matchLabels/matchExpressions) matches the pod
kubectl get networkpolicy -n "$NS" -o json \
| jq --argjson podLabels "$POD_LABELS_JSON" '
  .items[]
  | select(
      (
        # matchLabels: every key=value must be present in pod labels
        ((.spec.podSelector.matchLabels // {}) as $ml
         | all($ml|keys[]?; $podLabels[.] == $ml[.])
        )
        and
        # matchExpressions: support In/NotIn/Exists/DoesNotExist
        ((.spec.podSelector.matchExpressions // []) | all(.[]?;
          ( .operator == "In" and (($podLabels[.key] // "") as $v | any(.values[]?; . == $v)) )
          or ( .operator == "NotIn" and (($podLabels[.key] // "") as $v | all(.values[]?; . != $v)) )
          or ( .operator == "Exists" and ($podLabels[.key] != null) )
          or ( .operator == "DoesNotExist" and ($podLabels[.key] == null) )
        ))
      )
    )
  | .metadata.name
'

