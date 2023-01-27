import {} from './header.entrypoint.js'
import List from './List.root.svelte'

new List({
    target: document.getElementById('svelte-app'),
    props: {
        filterPresets: filterPresets,
        type: listType,
        project: project,
        isManager: isManager
    },
    context: new Map([
            ['cc-config', clusterCockpitConfig]
    ])
})
