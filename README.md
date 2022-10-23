# SolrCloud readiness blocking waiter

SolrCloud doesent expose node readiness condition in status, which can be used by wait/jobs.  
This utility blocks until requested cluster is ready. We compare `replicas` with `readyReplicas` in `status` of SolrCloud resource.  

If there is an easier way to do this (e.g. with some `kubectl wait --for=condition=[magic]` command) please let me know.


### Example usage
```sh
solr-waiter solr-test -n sandbox
ready 2 out of 3, retry in 3s, timeout in ~9m59s
ready 2 out of 3, retry in 3s, timeout in ~9m56s
cluster solr-test is ready. Exiting.
```

### Example usage in cluster
See example job in `test-job.yml`