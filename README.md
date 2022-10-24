# SolrCloud readiness blocking waiter

SolrCloud doesent expose node readiness condition in status, which can be used by wait/jobs.  
This utility blocks until requested cluster is ready. We compare `replicas` with `readyReplicas` in `status` of SolrCloud resource.  

If there is an easier way to do this (e.g. with some `kubectl wait --for=condition=[magic]` command) please let me know.


### Example usage
```sh
solr-waiter solr-test -n sandbox                                                                                        gke_personal/sandbox
elapsed 0s, ready 2 out of 3
elapsed 3s, still watching cluster (timeout in ~9m57s)
elapsed 4s, cluster solr-test is ready. Exiting.
```

### Example usage in cluster
See example job in `test-job.yml`