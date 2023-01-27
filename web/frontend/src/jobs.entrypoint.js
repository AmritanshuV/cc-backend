import {} from './header.entrypoint.js'
import Jobs from './Jobs.root.svelte'

new Jobs({
    target: document.getElementById('svelte-app'),
    props: {
        filterPresets: filterPresets,
        project: project,
        isManager: isManager
    },
    context: new Map([
            ['cc-config', clusterCockpitConfig]
    ])
})
